package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/gin-gonic/gin"
	"go.apps.applied.dev/lib/slacklib"
)

//go:generate sh -c "cd frontend && npm install && npm run build"
//go:embed frontend/dist
var frontendFS embed.FS

type Snippet struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type RecentRun struct {
	Run      string    `json:"run"`
	RunUUID  string    `json:"run_uuid"`
	RunDate  string    `json:"run_date"`
	Snippets []Snippet `json:"snippets"`
}

type FilterEntry struct {
	FilterName  string  `json:"filter_name"`
	FailSeconds int     `json:"fail_seconds"`
	FailPct     float64 `json:"fail_pct"`
}

type QualityRun struct {
	RunID      string        `json:"run_id"`
	RunUUID    string        `json:"run_uuid"`
	RunMinutes int           `json:"run_minutes"`
	Filters    []FilterEntry `json:"filters"`
}

type SkippedRun struct {
	RunUUID         string `json:"run_uuid"`
	DriveID         string `json:"drive_id"`
	RunDate         string `json:"run_date"`
	DurationMinutes int    `json:"duration_minutes"`
	Reason          string `json:"reason,omitempty"`
}

type ZeroSegmentRun struct {
	RunUUID         string        `json:"run_uuid"`
	RunID           string        `json:"run_id"`
	RunDate         string        `json:"run_date"`
	DurationMinutes int           `json:"duration_minutes"`
	Filters         []FilterEntry `json:"filters"`
}

type ConversionIssue struct {
	RunUUID         string `json:"run_uuid"`
	DriveID         string `json:"drive_id"`
	RunDate         string `json:"run_date"`
	DurationMinutes int    `json:"duration_minutes"`
	Reason          string `json:"reason,omitempty"`
}

type CameraCalibration struct {
	Camera         string `json:"camera"`
	LastCalibrated string `json:"last_calibrated"`
	DaysSince      int    `json:"days_since"`
	Stale          bool   `json:"stale"`
}

type TruckCamera struct {
	Name     string    `json:"name"`
	Group    string    `json:"group"` // "AT360" or "QT128"
	Snippets []Snippet `json:"snippets"`
}

type Truck struct {
	UUID    string        `json:"uuid"`
	Date    string        `json:"date"`
	Stale   bool          `json:"stale"`
	Cameras []TruckCamera `json:"cameras"`
}

type Vehicle struct {
	ID               string              `json:"id"`
	Project          string              `json:"project,omitempty"`
	Location         string              `json:"location,omitempty"`
	Run              string              `json:"run"`
	RunUUID          string              `json:"run_uuid,omitempty"`
	CamGen           string              `json:"cam_gen"`
	Snippets         []Snippet           `json:"snippets"`
	RecentRuns       []RecentRun         `json:"recent_runs"`
	Stale            bool                `json:"stale"`
	StaleReason      string              `json:"stale_reason,omitempty"`
	Inactive         bool                `json:"inactive,omitempty"`
	QualityRuns      []QualityRun        `json:"quality_runs"`
	SkippedRuns      []SkippedRun        `json:"skipped_runs,omitempty"`
	ZeroSegmentRuns  []ZeroSegmentRun    `json:"zero_segment_runs,omitempty"`
	ConversionIssues []ConversionIssue   `json:"conversion_issues,omitempty"`
	Calibrations     []CameraCalibration `json:"calibrations"`
}

var (
	vehicles   []Vehicle
	vehiclesMu sync.RWMutex

	trucks   []Truck
	trucksMu sync.RWMutex

	// jiraToken loaded at startup from env or Secret Manager.
	jiraToken string

	// slackBot initialized at startup via slacklib (optional, nil if not configured).
	slackBot *slacklib.Bot
)

const (
	jiraEmail  = "anshav.nepal@ext.applied.co"
	jiraBase   = "https://appliedintuition.atlassian.net"
	appVersion = "v1.5"
)

// secretNames maps the Secret Manager short name (without app prefix) to the
// env var name that runRefresh (refresh.go) reads.
var secretNames = map[string]string{
	"neuron-client-id":         "NEURON_CLIENT_ID",
	"neuron-client-secret":     "NEURON_CLIENT_SECRET",
	"oci-s3-access-key-id":     "OCI_S3_ACCESS_KEY_ID",
	"oci-s3-secret-access-key": "OCI_S3_SECRET_ACCESS_KEY",
	"oci-s3-endpoint-url":      "OCI_S3_ENDPOINT_URL",
}

func loadSecretsFromSecretManager() {
	projectID := os.Getenv("PROJECT_ID")
	if projectID == "" {
		log.Println("PROJECT_ID not set — skipping Secret Manager (local dev mode)")
		return
	}

	ctx := context.Background()
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		log.Printf("Warning: failed to create Secret Manager client: %v", err)
		return
	}
	defer client.Close()

	appName := os.Getenv("K_SERVICE")
	if appName == "" {
		appName = "onroad-biweekly"
	}

	loaded := 0
	for shortName, envVar := range secretNames {
		fullName := fmt.Sprintf("projects/%s/secrets/%s-%s/versions/latest", projectID, appName, shortName)
		result, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
			Name: fullName,
		})
		if err != nil {
			log.Printf("Warning: could not fetch secret %s: %v", fullName, err)
			continue
		}
		if err := os.Setenv(envVar, string(result.Payload.Data)); err != nil {
			log.Printf("Warning: could not set env var %s: %v", envVar, err)
			continue
		}
		loaded++
		log.Printf("Loaded secret → %s", envVar)
	}
	log.Printf("Loaded %d secret(s)", loaded)
}

func loadJiraToken() {
	if t := os.Getenv("JIRA_API_TOKEN"); t != "" {
		jiraToken = t
		log.Println("Loaded JIRA_API_TOKEN from env")
		return
	}
	projectID := os.Getenv("PROJECT_ID")
	if projectID == "" {
		log.Println("JIRA_API_TOKEN not set — Jira integration disabled in local dev")
		return
	}
	ctx := context.Background()
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		log.Printf("Warning: failed to create Secret Manager client for Jira: %v", err)
		return
	}
	defer client.Close()
	appName := os.Getenv("K_SERVICE")
	if appName == "" {
		appName = "onroad-biweekly"
	}
	name := fmt.Sprintf("projects/%s/secrets/%s-jira-api-token/versions/latest", projectID, appName)
	result, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		log.Printf("Warning: could not fetch Jira API token: %v", err)
		return
	}
	jiraToken = string(result.Payload.Data)
	log.Println("Loaded Jira API token from Secret Manager")
}

func initSlackBot() {
	slackBot = slacklib.New(slacklib.Config{})
	log.Println("Slack bot initialized")
}

// jiraLookupAccountID returns the Jira accountId for an email, or "" on error.
func jiraLookupAccountID(email string) string {
	if jiraToken == "" || email == "" {
		return ""
	}
	resp, err := jiraRequest("GET", "/rest/api/3/user/search?query="+email, nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var users []struct {
		AccountID string `json:"accountId"`
		EmailAddress string `json:"emailAddress"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil || len(users) == 0 {
		return ""
	}
	for _, u := range users {
		if strings.EqualFold(u.EmailAddress, email) {
			return u.AccountID
		}
	}
	return users[0].AccountID
}

// slackLookupUserByEmail returns the Slack user ID for an email, or "" on error.
func slackLookupUserByEmail(email string) string {
	if slackBot == nil || email == "" {
		return ""
	}
	client, err := slackBot.Client()
	if err != nil {
		return ""
	}
	user, err := client.GetUserByEmail(email)
	if err != nil {
		return ""
	}
	return user.ID
}

// postToSlack posts text to a Slack channel. No-ops if slackBot is nil.
func postToSlack(ctx context.Context, channel, text string) error {
	if slackBot == nil {
		return fmt.Errorf("Slack bot not initialized (token not found in Secret Manager)")
	}
	_, err := slackBot.SendMessage(ctx, channel, text)
	return err
}

func loadVehicles() {
	data, err := os.ReadFile("vehicles.json")
	if err != nil {
		log.Printf("vehicles.json not found, serving empty list (click Fetch Newest Runs to populate)")
		return
	}
	var v []Vehicle
	if err := json.Unmarshal(data, &v); err != nil {
		log.Fatalf("Failed to parse vehicles.json: %v", err)
	}
	vehiclesMu.Lock()
	vehicles = v
	vehiclesMu.Unlock()
	log.Printf("Loaded %d vehicles from vehicles.json", len(v))
}

func loadTrucks() {
	data, err := os.ReadFile("trucks.json")
	if err != nil {
		log.Printf("trucks.json not found, serving empty list (click Refresh Trucks to populate)")
		return
	}
	var t []Truck
	if err := json.Unmarshal(data, &t); err != nil {
		log.Printf("Warning: failed to parse trucks.json: %v", err)
		return
	}
	trucksMu.Lock()
	trucks = t
	trucksMu.Unlock()
	log.Printf("Loaded %d trucks from trucks.json", len(t))
}

// ---------------------------------------------------------------------------
// Jira helpers
// ---------------------------------------------------------------------------

// adfDoc / adfNode implement Atlassian Document Format for Jira descriptions.
type adfDoc struct {
	Type    string    `json:"type"`
	Version int       `json:"version"`
	Content []adfNode `json:"content"`
}

type adfNode struct {
	Type    string         `json:"type"`
	Content []adfNode      `json:"content,omitempty"`
	Text    string         `json:"text,omitempty"`
	Marks   []adfMark      `json:"marks,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

type adfMark struct {
	Type string `json:"type"`
}

func adfPara(nodes ...adfNode) adfNode {
	return adfNode{Type: "paragraph", Content: nodes}
}

func adfText(text string) adfNode { return adfNode{Type: "text", Text: text} }

func adfBold(text string) adfNode {
	return adfNode{Type: "text", Text: text, Marks: []adfMark{{Type: "strong"}}}
}

func adfBulletList(items ...adfNode) adfNode {
	return adfNode{Type: "bulletList", Content: items}
}

func adfListItem(text string) adfNode {
	return adfNode{Type: "listItem", Content: []adfNode{adfPara(adfText(text))}}
}

// maxJiraDescriptionSections bounds how many past "rule"-separated report
// sections we keep when appending, so the description can't grow forever
// and trip Jira's CONTENT_LIMIT_EXCEEDED error.
const maxJiraDescriptionSections = 5

// maxRunBreakdownItems caps how many per-run entries buildJiraTicket lists,
// so a single report for a high-run-count vehicle can't alone exceed Jira's
// description size limit.
const maxRunBreakdownItems = 15

// maxJiraDescriptionBytes is a safety ceiling on the final merged description
// payload (ADF JSON). Jira Cloud rejects descriptions past ~32,767 chars with
// CONTENT_LIMIT_EXCEEDED; we stay comfortably under that.
const maxJiraDescriptionBytes = 28000

// adfContentBytes returns the JSON-encoded size of an ADF content slice as
// it would be sent to Jira (wrapped in a doc envelope).
func adfContentBytes(content []adfNode) int {
	b, err := json.Marshal(adfDoc{Type: "doc", Version: 1, Content: content})
	if err != nil {
		return 0
	}
	return len(b)
}

// trimADFSections keeps only the last keepSections sections of content,
// where sections are separated by "rule" nodes (our divider between reports).
func trimADFSections(content []adfNode, keepSections int) []adfNode {
	if len(content) == 0 || keepSections <= 0 {
		return nil
	}
	var sections [][]adfNode
	var cur []adfNode
	for _, n := range content {
		if n.Type == "rule" {
			sections = append(sections, cur)
			cur = nil
			continue
		}
		cur = append(cur, n)
	}
	sections = append(sections, cur)
	if len(sections) > keepSections {
		sections = sections[len(sections)-keepSections:]
	}
	var out []adfNode
	for i, s := range sections {
		if i > 0 {
			out = append(out, adfNode{Type: "rule"})
		}
		out = append(out, s...)
	}
	return out
}

var vehicleIDRe = regexp.MustCompile(`^([a-zA-Z]+)(\d+)$`)
var runDateRe = regexp.MustCompile(`_(\d{4})(\d{2})(\d{2})_`)

// vehicleJiraID converts "rog103" → "ROG-103", "mce103" → "MCE-103".
func vehicleJiraID(id string) string {
	m := vehicleIDRe.FindStringSubmatch(id)
	if m != nil {
		return strings.ToUpper(m[1]) + "-" + m[2]
	}
	return strings.ToUpper(id)
}

// goFilterLabel mirrors the TypeScript filterLabel: strips _filter suffix, title-cases words.
func goFilterLabel(name string) string {
	name = strings.TrimSuffix(name, "_filter")
	words := strings.Split(name, "_")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func vehicleComponents(v Vehicle) []map[string]string {
	comps := []map[string]string{
		{"name": "Data Quality"},
		{"name": "On Road DC"},
	}
	if strings.Contains(v.Project, "Gen-2") || strings.Contains(v.Project, "Gen2") {
		comps = append(comps, map[string]string{"name": "Gen2"})
	} else {
		comps = append(comps, map[string]string{"name": "Gen1"})
	}
	return comps
}

// pluralS returns "s" when n != 1.
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// dateRangeFromRuns extracts the min and max drive dates from QualityRun IDs
// (format: prefix_YYYYMMDD_HHMMSS) and returns "MM/DD-MM/DD".
// Falls back to the last-7-days window when no parseable dates are found.
func dateRangeFromRuns(vs []Vehicle) string {
	var minDate, maxDate time.Time
	for _, v := range vs {
		for _, qr := range v.QualityRuns {
			m := runDateRe.FindStringSubmatch(qr.RunID)
			if m == nil {
				continue
			}
			t, err := time.Parse("20060102", m[1]+m[2]+m[3])
			if err != nil {
				continue
			}
			if minDate.IsZero() || t.Before(minDate) {
				minDate = t
			}
			if maxDate.IsZero() || t.After(maxDate) {
				maxDate = t
			}
		}
	}
	if minDate.IsZero() {
		today := time.Now()
		weekAgo := today.AddDate(0, 0, -7)
		return fmt.Sprintf("%s-%s", weekAgo.Format("01/02"), today.Format("01/02"))
	}
	if minDate.Equal(maxDate) {
		return minDate.Format("01/02")
	}
	return fmt.Sprintf("%s-%s", minDate.Format("01/02"), maxDate.Format("01/02"))
}

// vehicleGenLabel returns "Gen2 vehicles", "Gen1 vehicles", or "Gen1/Gen2 vehicles"
// based on the project labels of the provided vehicles.
func vehicleGenLabel(vs []Vehicle) string {
	hasGen2, hasGen1 := false, false
	for _, v := range vs {
		p := v.Project
		if strings.Contains(p, "Gen-2") || strings.Contains(p, "Gen2") {
			hasGen2 = true
		} else {
			hasGen1 = true
		}
	}
	if hasGen2 && hasGen1 {
		return "Gen1/Gen2 vehicles"
	}
	if hasGen2 {
		return "Gen2 vehicles"
	}
	return "Gen1 vehicles"
}

// extractCameraGroup maps a filter name to its camera/sensor group label.
func extractCameraGroup(filterName string) string {
	switch {
	case strings.Contains(filterName, "front_center_narrow_camera"):
		return "Front Center Narrow Camera"
	case strings.Contains(filterName, "front_center_camera"):
		return "Front Center Camera"
	case strings.Contains(filterName, "front_right_camera"):
		return "Front Right Camera"
	case strings.Contains(filterName, "front_left_camera"):
		return "Front Left Camera"
	case strings.Contains(filterName, "rear_center_camera"):
		return "Rear Center Camera"
	case strings.Contains(filterName, "rear_right_camera"):
		return "Rear Right Camera"
	case strings.Contains(filterName, "rear_left_camera"):
		return "Rear Left Camera"
	case strings.Contains(filterName, "multi_camera"):
		return "Multi-Camera"
	case strings.Contains(filterName, "hesai"):
		return "Hesai LiDAR"
	case strings.Contains(filterName, "lidar"):
		return "LiDAR"
	case strings.Contains(filterName, "motion") || strings.Contains(filterName, "heading") ||
		strings.Contains(filterName, "roll_angle") || strings.Contains(filterName, "pitch_angle"):
		return "IMU / Motion"
	case strings.Contains(filterName, "min_frames"):
		return "Frame Coverage"
	default:
		return "Other"
	}
}

// adfListItemWithNested creates a listItem with a header paragraph and an optional
// nested bullet list — used for the run → per-camera hierarchy in fleet tickets.
func adfListItemWithNested(header adfNode, subItems []adfNode) adfNode {
	content := []adfNode{adfPara(header)}
	if len(subItems) > 0 {
		content = append(content, adfBulletList(subItems...))
	}
	return adfNode{Type: "listItem", Content: content}
}

// cameraFilterSuffix returns the type-family suffix after "_camera_" in a filter name.
// "front_center_camera_qalign_solar_conditioned_filter" → "qalign_solar_conditioned_filter"
// Returns "" when the filter name does not follow the camera-filter pattern.
func cameraFilterSuffix(filterName string) string {
	idx := strings.Index(filterName, "_camera_")
	if idx < 0 {
		return ""
	}
	return filterName[idx+len("_camera_"):]
}

type filterAgg struct {
	name    string
	failSec int
}

func buildJiraTicket(v Vehicle) (summary string, doc adfDoc) {
	// Aggregate filter fail seconds across all runs
	aggMap := map[string]*filterAgg{}
	totalRunSec := 0
	for _, qr := range v.QualityRuns {
		runSec := qr.RunMinutes * 60
		totalRunSec += runSec
		for _, f := range qr.Filters {
			if aggMap[f.FilterName] == nil {
				aggMap[f.FilterName] = &filterAgg{name: f.FilterName}
			}
			aggMap[f.FilterName].failSec += f.FailSeconds
		}
	}

	// Sort filters by total fail seconds descending
	filters := make([]*filterAgg, 0, len(aggMap))
	for _, f := range aggMap {
		filters = append(filters, f)
	}
	sort.Slice(filters, func(i, j int) bool { return filters[i].failSec > filters[j].failSec })

	// Build summary: [ROG-103] - Filter1 + Filter2 filtering
	topLabels := make([]string, 0, 3)
	for _, f := range filters {
		if len(topLabels) >= 3 {
			break
		}
		topLabels = append(topLabels, goFilterLabel(f.name))
	}
	vid := vehicleJiraID(v.ID)
	if len(topLabels) == 0 {
		summary = fmt.Sprintf("[%s] - Data quality filtering", vid)
	} else {
		summary = fmt.Sprintf("[%s] - %s filtering", vid, strings.Join(topLabels, " + "))
	}

	// Date window
	today := time.Now()
	weekAgo := today.AddDate(0, 0, -7)
	window := fmt.Sprintf("%s-%s", weekAgo.Format("01/02"), today.Format("01/02"))

	nodes := []adfNode{}

	// Intro
	nodes = append(nodes, adfPara(adfText(fmt.Sprintf(
		"%s showed significant data-quality filtering during the %s reporting window "+
			"(ORDC Operations Dashboard, Quality Filters tab). Fail-rate = filter fail-hours / vehicle recording-hours.",
		vid, window,
	))))

	// Vehicle-level impact
	if len(filters) > 0 && totalRunSec > 0 {
		nodes = append(nodes, adfPara(adfBold("Vehicle-level impact (share of recording hours):")))
		items := make([]adfNode, 0, len(filters))
		for _, f := range filters {
			pct := float64(f.failSec) / float64(totalRunSec) * 100
			items = append(items, adfListItem(fmt.Sprintf("%s: %.1f%%", goFilterLabel(f.name), pct)))
		}
		nodes = append(nodes, adfBulletList(items...))
	}

	// Per-run breakdown (capped so vehicles with many runs don't blow past
	// Jira's description size limit; ranked by fail-time impact).
	if len(v.QualityRuns) > 0 {
		type runLine struct {
			text    string
			failSec int
		}
		var lines []runLine
		for _, qr := range v.QualityRuns {
			if len(qr.Filters) == 0 {
				continue
			}
			parts := make([]string, 0, len(qr.Filters))
			runFailSec := 0
			for _, f := range qr.Filters {
				parts = append(parts, fmt.Sprintf("%s: %.1f%%", goFilterLabel(f.FilterName), f.FailPct))
				runFailSec += f.FailSeconds
			}
			lines = append(lines, runLine{
				text:    fmt.Sprintf("%s (%d min) — %s", qr.RunID, qr.RunMinutes, strings.Join(parts, ", ")),
				failSec: runFailSec,
			})
		}
		sort.Slice(lines, func(i, j int) bool { return lines[i].failSec > lines[j].failSec })
		omitted := 0
		if len(lines) > maxRunBreakdownItems {
			omitted = len(lines) - maxRunBreakdownItems
			lines = lines[:maxRunBreakdownItems]
		}
		if len(lines) > 0 {
			nodes = append(nodes, adfPara(adfBold("Run breakdown (last 7 days):")))
			runItems := make([]adfNode, 0, len(lines))
			for _, l := range lines {
				runItems = append(runItems, adfListItem(l.text))
			}
			nodes = append(nodes, adfBulletList(runItems...))
			if omitted > 0 {
				nodes = append(nodes, adfPara(adfText(fmt.Sprintf(
					"(+%d more runs not shown, ranked by fail-time impact — see dashboard for full breakdown)", omitted,
				))))
			}
		}
	}

	// Footer
	nodes = append(nodes, adfPara(adfText(fmt.Sprintf(
		"Source: ORDC Operations Dashboard, Quality Filters tab. Window %s. "+
			"record_pause, slam_quality, and garage filters excluded.",
		window,
	))))

	doc = adfDoc{Type: "doc", Version: 1, Content: nodes}
	return
}

// ---------------------------------------------------------------------------
// Fleet filter aggregation
// ---------------------------------------------------------------------------

type FleetFilter struct {
	FilterName         string   `json:"filter_name"`
	FilterLabel        string   `json:"filter_label"`
	AffectedVehicleIDs []string `json:"affected_vehicle_ids"`
	TotalFailSeconds   int      `json:"total_fail_seconds"`
	VehicleCount       int      `json:"vehicle_count"`
}

func getFleetFilters() []FleetFilter {
	vehiclesMu.RLock()
	vs := vehicles
	vehiclesMu.RUnlock()

	type agg struct {
		failSecByVehicle map[string]int
		totalFailSec     int
	}
	aggMap := map[string]*agg{}
	for _, v := range vs {
		for _, qr := range v.QualityRuns {
			for _, f := range qr.Filters {
				if aggMap[f.FilterName] == nil {
					aggMap[f.FilterName] = &agg{failSecByVehicle: map[string]int{}}
				}
				aggMap[f.FilterName].failSecByVehicle[v.ID] += f.FailSeconds
				aggMap[f.FilterName].totalFailSec += f.FailSeconds
			}
		}
	}

	filters := []FleetFilter{}
	for name, a := range aggMap {
		if len(a.failSecByVehicle) < 2 {
			continue
		}
		vIDs := make([]string, 0, len(a.failSecByVehicle))
		for vid := range a.failSecByVehicle {
			vIDs = append(vIDs, vid)
		}
		sort.Strings(vIDs)
		filters = append(filters, FleetFilter{
			FilterName:         name,
			FilterLabel:        goFilterLabel(name),
			AffectedVehicleIDs: vIDs,
			TotalFailSeconds:   a.totalFailSec,
			VehicleCount:       len(a.failSecByVehicle),
		})
	}
	sort.Slice(filters, func(i, j int) bool {
		if filters[i].VehicleCount != filters[j].VehicleCount {
			return filters[i].VehicleCount > filters[j].VehicleCount
		}
		return filters[i].TotalFailSeconds > filters[j].TotalFailSeconds
	})
	return filters
}

func isQalignFilter(name string) bool {
	return strings.Contains(strings.ToLower(name), "qalign")
}

func buildFleetJiraTicket(filterName string, affectedVehicles []Vehicle) (summary string, doc adfDoc) {
	genLabel := vehicleGenLabel(affectedVehicles)
	dateRange := dateRangeFromRuns(affectedVehicles)

	if isQalignFilter(filterName) {
		return buildFleetQalignTicket(filterName, affectedVehicles, dateRange, genLabel)
	}

	label := goFilterLabel(filterName)

	// Summary: [Fleet] - {label} filtering across {genLabel} ({dateRange})
	summary = fmt.Sprintf("[Fleet] - %s filtering across %s (%s)", label, genLabel, dateRange)
	if len(summary) > 255 {
		summary = summary[:252] + "..."
	}

	nodes := []adfNode{}

	// Intro paragraph.
	nodes = append(nodes, adfPara(adfText(fmt.Sprintf(
		"%s filtering observed across %d %s during %s (ORDC Operations Dashboard, Quality Filters tab).",
		label, len(affectedVehicles), genLabel, dateRange,
	))))

	// targetSuffix is non-empty for camera fleet filters; it is the filter-type
	// family (everything after "_camera_") used to match all camera variants in a run.
	targetSuffix := cameraFilterSuffix(filterName)

	// Per-vehicle → per-run → per-camera breakdown.
	// Sort vehicles by ID for consistent ordering.
	sortedVehicles := make([]Vehicle, len(affectedVehicles))
	copy(sortedVehicles, affectedVehicles)
	sort.Slice(sortedVehicles, func(i, j int) bool {
		return sortedVehicles[i].ID < sortedVehicles[j].ID
	})

	for _, v := range sortedVehicles {
		vid := vehicleJiraID(v.ID)

		// Count runs that contain the target filter (to annotate the vehicle header).
		runsWithFilter := 0
		for _, qr := range v.QualityRuns {
			for _, f := range qr.Filters {
				if f.FilterName == filterName {
					runsWithFilter++
					break
				}
			}
		}
		if runsWithFilter == 0 {
			continue
		}
		nodes = append(nodes, adfPara(adfBold(
			fmt.Sprintf("%s (%d run%s):", vid, runsWithFilter, pluralS(runsWithFilter)),
		)))

		runItems := make([]adfNode, 0)
		for _, qr := range v.QualityRuns {
			if len(qr.Filters) == 0 {
				continue
			}
			// Only include runs that actually hit the target filter.
			hasTarget := false
			for _, f := range qr.Filters {
				if f.FilterName == filterName {
					hasTarget = true
					break
				}
			}
			if !hasTarget {
				continue
			}
			// Group type-family filters by camera. For camera fleet filters only
			// include camera variants of the same filter type; for non-camera fleet
			// filters show just the exact filter match.
			type camGroup struct {
				entries []FilterEntry
			}
			camMap := map[string]*camGroup{}
			camOrder := []string{}
			for _, f := range qr.Filters {
				if targetSuffix != "" {
					// Camera fleet filter: include all camera variants of the same type.
					if !strings.Contains(f.FilterName, "_camera_"+targetSuffix) {
						continue
					}
				} else {
					// Non-camera fleet filter: exact match only.
					if f.FilterName != filterName {
						continue
					}
				}
				cam := extractCameraGroup(f.FilterName)
				if _, exists := camMap[cam]; !exists {
					camMap[cam] = &camGroup{}
					camOrder = append(camOrder, cam)
				}
				camMap[cam].entries = append(camMap[cam].entries, f)
			}
			if len(camOrder) == 0 {
				continue // run has no relevant camera entries
			}
			sort.Strings(camOrder)

			// One bullet per camera: single-filter groups are condensed to
			// "{Camera Label}: {fail_pct}%"; multi-filter groups expand per filter.
			camItems := make([]adfNode, 0, len(camOrder))
			for _, cam := range camOrder {
				grp := camMap[cam]
				if len(grp.entries) == 1 {
					camItems = append(camItems, adfListItem(
						fmt.Sprintf("%s: %.1f%%", cam, grp.entries[0].FailPct),
					))
				} else {
					filterParts := make([]string, 0, len(grp.entries))
					for _, f := range grp.entries {
						filterParts = append(filterParts, fmt.Sprintf("%s: %.1f%%", goFilterLabel(f.FilterName), f.FailPct))
					}
					camItems = append(camItems, adfListItem(
						fmt.Sprintf("%s — %s", cam, strings.Join(filterParts, ", ")),
					))
				}
			}

			runHeader := adfText(fmt.Sprintf("Run %s (%d min):", qr.RunID, qr.RunMinutes))
			runItems = append(runItems, adfListItemWithNested(runHeader, camItems))
		}
		if len(runItems) > 0 {
			nodes = append(nodes, adfBulletList(runItems...))
		}
	}

	// Footer.
	nodes = append(nodes, adfPara(adfText(fmt.Sprintf(
		"Source: ORDC Operations Dashboard, Quality Filters tab. Window %s. record_pause, slam_quality, and garage filters excluded.",
		dateRange,
	))))

	doc = adfDoc{Type: "doc", Version: 1, Content: nodes}
	return
}

// buildFleetQalignTicket generates the fleet ticket for camera QAlign filters.
// Summary: [Fleet] - Front/multi-camera QAlign filtering across {genLabel} ({window})
// Description: per-vehicle → per-run → per-camera fail rates using per-run FailPct values.
func buildFleetQalignTicket(triggerFilter string, affectedVehicles []Vehicle, window, genLabel string) (summary string, doc adfDoc) {
	// Decide summary label based on how many distinct qalign filter names appear.
	qalignNames := map[string]bool{}
	for _, v := range affectedVehicles {
		for _, qr := range v.QualityRuns {
			for _, f := range qr.Filters {
				if isQalignFilter(f.FilterName) {
					qalignNames[f.FilterName] = true
				}
			}
		}
	}
	summaryLabel := goFilterLabel(triggerFilter)
	if len(qalignNames) > 1 {
		summaryLabel = "Front/multi-camera QAlign"
	}
	summary = fmt.Sprintf("[Fleet] - %s filtering across %s (%s)", summaryLabel, genLabel, window)

	nodes := []adfNode{}
	nodes = append(nodes, adfPara(adfText(fmt.Sprintf(
		"%s on-road vehicles showed camera QAlign filtering during the %s reporting window "+
			"(ORDC Operations Dashboard, Quality Filters tab). "+
			"Fail-rate = filter fail-seconds / run recording-seconds.",
		genLabel, window,
	))))

	// Sort vehicles by total qalign fail seconds descending (most impacted first).
	sortedVehicles := make([]Vehicle, len(affectedVehicles))
	copy(sortedVehicles, affectedVehicles)
	sort.Slice(sortedVehicles, func(i, j int) bool {
		totI, totJ := 0, 0
		for _, qr := range sortedVehicles[i].QualityRuns {
			for _, f := range qr.Filters {
				if isQalignFilter(f.FilterName) {
					totI += f.FailSeconds
				}
			}
		}
		for _, qr := range sortedVehicles[j].QualityRuns {
			for _, f := range qr.Filters {
				if isQalignFilter(f.FilterName) {
					totJ += f.FailSeconds
				}
			}
		}
		return totI > totJ
	})

	for _, v := range sortedVehicles {
		// Count runs that had any qalign hit.
		runsWithQalign := 0
		for _, qr := range v.QualityRuns {
			for _, f := range qr.Filters {
				if isQalignFilter(f.FilterName) {
					runsWithQalign++
					break
				}
			}
		}
		if runsWithQalign == 0 {
			continue
		}

		vid := vehicleJiraID(v.ID)
		nodes = append(nodes, adfPara(adfBold(
			fmt.Sprintf("%s (%d run%s):", vid, runsWithQalign, pluralS(runsWithQalign)),
		)))

		// Per-run → per-camera breakdown using per-run FailPct values.
		runItems := make([]adfNode, 0)
		for _, qr := range v.QualityRuns {
			type camEntry struct {
				cam     string
				failPct float64
			}
			var entries []camEntry
			for _, f := range qr.Filters {
				if !isQalignFilter(f.FilterName) {
					continue
				}
				entries = append(entries, camEntry{
					cam:     extractCameraGroup(f.FilterName),
					failPct: f.FailPct,
				})
			}
			if len(entries) == 0 {
				continue
			}
			// Sort cameras alphabetically for consistent ordering.
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].cam < entries[j].cam
			})
			// One sub-bullet per camera: "{Camera Label}: {fail_pct}%"
			camItems := make([]adfNode, 0, len(entries))
			for _, e := range entries {
				camItems = append(camItems, adfListItem(
					fmt.Sprintf("%s: %.1f%%", e.cam, e.failPct),
				))
			}
			runHeader := adfText(fmt.Sprintf("Run %s (%d min):", qr.RunID, qr.RunMinutes))
			runItems = append(runItems, adfListItemWithNested(runHeader, camItems))
		}
		if len(runItems) > 0 {
			nodes = append(nodes, adfBulletList(runItems...))
		}
	}

	nodes = append(nodes, adfPara(adfText(fmt.Sprintf(
		"Source: ORDC Operations Dashboard, Quality Filters tab. Window %s. "+
			"record_pause, slam_quality, and garage filters excluded.",
		window,
	))))
	doc = adfDoc{Type: "doc", Version: 1, Content: nodes}
	return
}

func jiraAuthHeader() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(jiraEmail+":"+jiraToken))
}

func jiraRequest(method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, jiraBase+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", jiraAuthHeader())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return http.DefaultClient.Do(req)
}

// jiraDeleteIssue deletes a Jira issue by key (used to roll back on Slack failure).
func jiraDeleteIssue(key string) {
	resp, err := jiraRequest("DELETE", "/rest/api/3/issue/"+key, nil)
	if err != nil {
		log.Printf("rollback: failed to delete Jira issue %s: %v", key, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		log.Printf("rollback: deleted Jira issue %s", key)
	} else {
		log.Printf("rollback: unexpected status %d deleting Jira issue %s", resp.StatusCode, key)
	}
}

// findVehicle looks up a vehicle by ID under the read lock.
func findVehicle(id string) *Vehicle {
	vehiclesMu.RLock()
	defer vehiclesMu.RUnlock()
	for i := range vehicles {
		if vehicles[i].ID == id {
			v := vehicles[i]
			return &v
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// VSTAB Jira helpers
// ---------------------------------------------------------------------------

var vstabVehicleIDRe = regexp.MustCompile(`^([A-Z]+(?:-\d+)?)[,\s\-]+(.+)`)

type vstabTicket struct {
	Key       string
	VehicleID string
	Summary   string
	URL       string
}

func vstabFetchTickets() ([]vstabTicket, error) {
	jql := `project = VSTAB AND reporter in ("anshav.nepal@ext.applied.co", "adriel.naranjo@applied.co", "martin.meinke@applied.co") AND status != Closed ORDER BY created DESC`
	body := map[string]any{
		"jql":        jql,
		"fields":     []string{"summary", "key", "status"},
		"maxResults": 100,
	}
	resp, err := jiraRequest("POST", "/rest/api/3/search/jql", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("jira search %d: %s", resp.StatusCode, b)
	}
	var result struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	tickets := make([]vstabTicket, 0, len(result.Issues))
	for _, issue := range result.Issues {
		t := vstabTicket{
			Key: issue.Key,
			URL: jiraBase + "/browse/" + issue.Key,
		}
		if m := vstabVehicleIDRe.FindStringSubmatch(issue.Fields.Summary); m != nil {
			t.VehicleID = m[1]
			t.Summary = strings.TrimSpace(m[2])
		} else {
			t.Summary = issue.Fields.Summary
		}
		tickets = append(tickets, t)
	}
	return tickets, nil
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	loadSecretsFromSecretManager()
	loadJiraToken()
	initSlackBot()
	loadVehicles()
	loadTrucks()
	initDB()

	r := gin.Default()

	api := r.Group("/api")
	{
		api.GET("/version", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"version": appVersion})
		})

		api.GET("/vehicles", func(c *gin.Context) {
			vehiclesMu.RLock()
			v := vehicles
			vehiclesMu.RUnlock()
			c.JSON(http.StatusOK, v)
		})

		api.POST("/refresh", func(c *gin.Context) {
			log.Println("Starting data refresh...")
			fresh, err := runRefresh()
			if err != nil {
				log.Printf("refresh failed: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			vehiclesMu.Lock()
			vehicles = fresh
			vehiclesMu.Unlock()
			vehiclesMu.RLock()
			v := vehicles
			vehiclesMu.RUnlock()
			c.JSON(http.StatusOK, v)
		})

		api.GET("/trucks", func(c *gin.Context) {
			trucksMu.RLock()
			t := trucks
			trucksMu.RUnlock()
			if t == nil {
				t = []Truck{}
			}
			c.JSON(http.StatusOK, t)
		})

		api.POST("/trucks/refresh", func(c *gin.Context) {
			log.Println("Starting truck data refresh...")
			fresh, err := runRefreshTrucks()
			if err != nil {
				log.Printf("truck refresh failed: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			trucksMu.Lock()
			trucks = fresh
			trucksMu.Unlock()
			trucksMu.RLock()
			t := trucks
			trucksMu.RUnlock()
			c.JSON(http.StatusOK, t)
		})

		// POST /api/jira/create  body: {"vehicle_id":"rog103","title":"optional override"}
		api.POST("/jira/create", func(c *gin.Context) {
			if jiraToken == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jira integration not configured — set JIRA_API_TOKEN env var"})
				return
			}
			var req struct {
				VehicleID string `json:"vehicle_id"`
				Title     string `json:"title"`
			}
			if err := c.BindJSON(&req); err != nil || req.VehicleID == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "vehicle_id required"})
				return
			}
			v := findVehicle(req.VehicleID)
			if v == nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "vehicle not found"})
				return
			}
			summary, doc := buildJiraTicket(*v)
			if strings.TrimSpace(req.Title) != "" {
				summary = strings.TrimSpace(req.Title)
			}
			payload := map[string]any{
				"fields": map[string]any{
					"project":     map[string]string{"key": "VSTAB"},
					"issuetype":   map[string]string{"name": "Vehicle Stability Issue Report"},
					"summary":     summary,
					"priority":    map[string]string{"name": "P1"},
					"components":  vehicleComponents(*v),
					"description": doc,
				},
			}
			resp, err := jiraRequest("POST", "/rest/api/3/issue", payload)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusCreated {
				log.Printf("Jira create error %d: %s", resp.StatusCode, body)
				c.JSON(resp.StatusCode, gin.H{"error": "Jira API error", "detail": string(body)})
				return
			}
			var result map[string]any
			json.Unmarshal(body, &result)
			key, _ := result["key"].(string)
			c.JSON(http.StatusOK, gin.H{
				"key": key,
				"url": fmt.Sprintf("%s/browse/%s", jiraBase, key),
			})
		})

		// GET /api/me — returns the logged-in user's email from the IAP header.
		api.GET("/me", func(c *gin.Context) {
			email := ""
			if iap := c.GetHeader("X-Goog-Authenticated-User-Email"); iap != "" {
				if idx := strings.Index(iap, ":"); idx >= 0 {
					email = iap[idx+1:]
				} else {
					email = iap
				}
			}
			c.JSON(http.StatusOK, gin.H{"email": email})
		})

		// POST /api/triage/create-ticket  body: {vehicle, issue, description, drive_id}
		// Creates a P2 VSTAB ticket and optionally posts to #eng-sds-data-qa.
		api.POST("/triage/create-ticket", func(c *gin.Context) {
			// Recover from any panics so the client always gets JSON, never HTML.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("create-ticket: panic: %v", r)
					c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("internal error: %v", r)})
				}
			}()

			if jiraToken == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jira integration not configured"})
				return
			}
			var req struct {
				Vehicle     string `json:"vehicle"`
				Issue       string `json:"issue"`
				Description string `json:"description"`
				DriveID     string `json:"drive_id"`
			}
			if err := c.ShouldBindJSON(&req); err != nil || req.Vehicle == "" || req.Issue == "" || req.Description == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "vehicle, issue, and description are required"})
				return
			}

			// Pre-flight: verify Slack client is accessible before creating the Jira ticket.
			if slackBot == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Slack bot not initialized — token not found in Secret Manager"})
				return
			}
			if _, slackClientErr := slackBot.Client(); slackClientErr != nil {
				log.Printf("create-ticket: Slack client unavailable: %v", slackClientErr)
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Cannot reach Slack: " + slackClientErr.Error()})
				return
			}

			// Resolve the actual logged-in user from IAP header.
			var reporterEmail string
			if iap := c.GetHeader("X-Goog-Authenticated-User-Email"); iap != "" {
				if idx := strings.Index(iap, ":"); idx >= 0 {
					reporterEmail = iap[idx+1:]
				} else {
					reporterEmail = iap
				}
			}

			vid := vehicleJiraID(req.Vehicle)
			summary := fmt.Sprintf("[%s] - %s", vid, req.Issue)
			descNodes := []adfNode{adfPara(adfText(req.Description))}
			if req.DriveID != "" {
				descNodes = append(descNodes, adfPara(adfText("Drive ID: "+req.DriveID)))
			}
			doc := adfDoc{Type: "doc", Version: 1, Content: descNodes}
			fields := map[string]any{
				"project":           map[string]string{"key": "VSTAB"},
				"issuetype":         map[string]any{"id": "11470"},
				"summary":           summary,
				"priority":          map[string]string{"name": "P2"},
				"assignee":          map[string]string{"accountId": "712020:34cadc85-b4d0-4e2d-826f-d3d98806cec6"},
				"customfield_11114": []map[string]string{{"value": vid}},
				"description":       doc,
			}
			if reporterEmail != "" {
				if accountID := jiraLookupAccountID(reporterEmail); accountID != "" {
					fields["reporter"] = map[string]string{"accountId": accountID}
				}
			}
			payload := map[string]any{"fields": fields}
			resp, err := jiraRequest("POST", "/rest/api/3/issue", payload)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusCreated {
				log.Printf("triage create-ticket Jira error %d: %s", resp.StatusCode, body)
				c.JSON(resp.StatusCode, gin.H{"error": "Jira API error", "detail": string(body)})
				return
			}
			var jiraResult map[string]any
			json.Unmarshal(body, &jiraResult)
			key, _ := jiraResult["key"].(string)
			ticketURL := fmt.Sprintf("%s/browse/%s", jiraBase, key)

			// Build and post the Slack message. If this fails, roll back the Jira ticket.
			reporterMention := "<@U097WC3GQ3D>"
			if reporterEmail != "" {
				if uid := slackLookupUserByEmail(reporterEmail); uid != "" {
					reporterMention = "<@" + uid + ">"
				}
			}
			var sb strings.Builder
			sb.WriteString(":letter-white-exclamation::letter-white-exclamation: *NEW VEHICLE ISSUE* :letter-white-exclamation::letter-white-exclamation:\n")
			sb.WriteString("*Reporter:* " + reporterMention + "\n")
			sb.WriteString(fmt.Sprintf("*Vehicle:* %s\n\n", vid))
			sb.WriteString("*Triage DRI:* <@U0ALAPJ2QRE> <@U0AQGUHHV9N>\n")
			sb.WriteString("*CC:* <!subteam^S08KK1SSQ3Z> <@U08Q5N7BQ1G>\n")
			sb.WriteString(fmt.Sprintf("*Issue Summary:* %s\n", summary))
			sb.WriteString(fmt.Sprintf("*Issue Description:* %s\n", req.Description))
			if req.DriveID != "" {
				sb.WriteString(fmt.Sprintf("*Drive ID:* %s\n", req.DriveID))
			}
			sb.WriteString(fmt.Sprintf("*JIRA Ticket:* %s\n\n", ticketURL))
			sb.WriteString("Open the JIRA and comment an image of your screen!")
			_, slackPostErr := slackBot.SendMessage(c.Request.Context(), "C081FNNUQAY", sb.String())
			if slackPostErr != nil {
				log.Printf("create-ticket: Slack post failed, rolling back Jira issue %s: %v", key, slackPostErr)
				jiraDeleteIssue(key)
				c.JSON(http.StatusBadGateway, gin.H{"error": "Could not post to #eng-sds-data-qa: " + slackPostErr.Error() + ". Ticket was not created."})
				return
			}

			// Bust camera-tickets cache so new CQ badge shows immediately.
			camTicketsCacheMu.Lock()
			camTicketsCacheAt = time.Time{}
			camTicketsCacheMu.Unlock()

			c.JSON(http.StatusOK, gin.H{"key": key, "url": ticketURL})
		})

		// GET /api/jira/search?vehicle=rog103
		api.GET("/jira/search", func(c *gin.Context) {
			if jiraToken == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jira integration not configured"})
				return
			}
			vehicleID := c.Query("vehicle")
			if vehicleID == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "vehicle param required"})
				return
			}
			jiraID := vehicleJiraID(vehicleID)
			jql := fmt.Sprintf(`project=VSTAB AND summary~"[%s]" ORDER BY updated DESC`, jiraID)
			payload := map[string]any{
				"jql":        jql,
				"maxResults": 5,
				"fields":     []string{"summary", "status", "updated"},
			}
			resp, err := jiraRequest("POST", "/rest/api/3/search/jql", payload)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				c.JSON(resp.StatusCode, gin.H{"error": "Jira API error", "detail": string(body)})
				return
			}
			var result struct {
				Issues []struct {
					Key    string `json:"key"`
					Fields struct {
						Summary string `json:"summary"`
						Status  struct {
							Name string `json:"name"`
						} `json:"status"`
						Updated string `json:"updated"`
					} `json:"fields"`
				} `json:"issues"`
			}
			json.Unmarshal(body, &result)
			tickets := make([]gin.H, 0, len(result.Issues))
			for _, issue := range result.Issues {
				tickets = append(tickets, gin.H{
					"key":     issue.Key,
					"summary": issue.Fields.Summary,
					"status":  issue.Fields.Status.Name,
					"updated": issue.Fields.Updated,
					"url":     fmt.Sprintf("%s/browse/%s", jiraBase, issue.Key),
				})
			}
			c.JSON(http.StatusOK, gin.H{"tickets": tickets})
		})

		// POST /api/jira/refresh-cache
		// Force-invalidates the camera-tickets in-memory cache and re-fetches from Jira.
		// Use this instead of GET /api/triage/camera-tickets when the user wants fresh data
		// immediately (e.g. after creating a new ticket) without waiting for the 5-min TTL.
		api.POST("/jira/refresh-cache", func(c *gin.Context) {
			if jiraToken == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jira integration not configured — set JIRA_API_TOKEN"})
				return
			}
			tickets, err := vstabFetchTickets()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			var lines []string
			for _, t := range tickets {
				if !strings.Contains(strings.ToLower(t.Summary), "camera image quality") {
					continue
				}
				line := fmt.Sprintf("[%s] - %s | %s", t.VehicleID, t.Summary, t.URL)
				if t.VehicleID == "" {
					line = t.Summary + " | " + t.URL
				}
				lines = append(lines, line)
			}
			content := strings.Join(lines, "\n")

			camTicketsCacheMu.Lock()
			camTicketsCache = content
			camTicketsCacheAt = time.Now()
			camTicketsCacheMu.Unlock()

			log.Printf("jira/refresh-cache: cache refreshed, %d camera-quality ticket(s)", len(lines))
			c.JSON(http.StatusOK, gin.H{"content": content})
		})

		// GET /api/fleet/filters
		api.GET("/fleet/filters", func(c *gin.Context) {
			c.JSON(http.StatusOK, getFleetFilters())
		})

		// GET /api/jira/fleet/search  (returns all [Fleet] tickets, filter param unused but kept for compat)
		api.GET("/jira/fleet/search", func(c *gin.Context) {
			if jiraToken == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jira integration not configured"})
				return
			}
			// Search all [Fleet] tickets — human-written summaries don't match computed filter labels,
			// so we surface all and let the user pick the right one.
			jql := `project=VSTAB AND summary~"Fleet" ORDER BY updated DESC`
			_ = c.Query("filter")
			payload := map[string]any{
				"jql":        jql,
				"maxResults": 25,
				"fields":     []string{"summary", "status", "updated"},
			}
			resp, err := jiraRequest("POST", "/rest/api/3/search/jql", payload)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				c.JSON(resp.StatusCode, gin.H{"error": "Jira API error", "detail": string(body)})
				return
			}
			var result struct {
				Issues []struct {
					Key    string `json:"key"`
					Fields struct {
						Summary string `json:"summary"`
						Status  struct {
							Name string `json:"name"`
						} `json:"status"`
						Updated string `json:"updated"`
					} `json:"fields"`
				} `json:"issues"`
			}
			json.Unmarshal(body, &result)
			tickets := make([]gin.H, 0, len(result.Issues))
			for _, issue := range result.Issues {
				tickets = append(tickets, gin.H{
					"key":     issue.Key,
					"summary": issue.Fields.Summary,
					"status":  issue.Fields.Status.Name,
					"updated": issue.Fields.Updated,
					"url":     fmt.Sprintf("%s/browse/%s", jiraBase, issue.Key),
				})
			}
			c.JSON(http.StatusOK, gin.H{"tickets": tickets})
		})

		// POST /api/jira/fleet/create  body: {"filter_name":"hesai_time_delta_filter","vehicle_ids":["rog103","mce101"]}
		api.POST("/jira/fleet/create", func(c *gin.Context) {
			if jiraToken == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jira integration not configured"})
				return
			}
			var req struct {
				FilterName string   `json:"filter_name"`
				VehicleIDs []string `json:"vehicle_ids"`
			}
			if err := c.BindJSON(&req); err != nil || req.FilterName == "" || len(req.VehicleIDs) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "filter_name and vehicle_ids required"})
				return
			}
			vehiclesMu.RLock()
			var affectedVehicles []Vehicle
			for _, vid := range req.VehicleIDs {
				for i := range vehicles {
					if vehicles[i].ID == vid {
						affectedVehicles = append(affectedVehicles, vehicles[i])
						break
					}
				}
			}
			vehiclesMu.RUnlock()
			if len(affectedVehicles) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "no matching vehicles found"})
				return
			}
			summary, doc := buildFleetJiraTicket(req.FilterName, affectedVehicles)
			payload := map[string]any{
				"fields": map[string]any{
					"project":     map[string]string{"key": "VSTAB"},
					"issuetype":   map[string]string{"name": "Vehicle Stability Issue Report"},
					"summary":     summary,
					"priority":    map[string]string{"name": "P1"},
					"components":  []map[string]string{{"name": "Data Quality"}, {"name": "On Road DC"}},
					"description": doc,
				},
			}
			resp, err := jiraRequest("POST", "/rest/api/3/issue", payload)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusCreated {
				log.Printf("Jira fleet create error %d: %s", resp.StatusCode, body)
				c.JSON(resp.StatusCode, gin.H{"error": "Jira API error", "detail": string(body)})
				return
			}
			var result map[string]any
			json.Unmarshal(body, &result)
			key, _ := result["key"].(string)
			c.JSON(http.StatusOK, gin.H{
				"key": key,
				"url": fmt.Sprintf("%s/browse/%s", jiraBase, key),
			})
		})

		// PUT /api/jira/fleet/update/:key  body: {"filter_name":"...","vehicle_ids":["rog103",...]}
		api.PUT("/jira/fleet/update/:key", func(c *gin.Context) {
			if jiraToken == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jira integration not configured"})
				return
			}
			issueKey := c.Param("key")
			var req struct {
				FilterName string   `json:"filter_name"`
				VehicleIDs []string `json:"vehicle_ids"`
			}
			if err := c.BindJSON(&req); err != nil || req.FilterName == "" || len(req.VehicleIDs) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "filter_name and vehicle_ids required"})
				return
			}
			vehiclesMu.RLock()
			var affectedVehicles []Vehicle
			for _, vid := range req.VehicleIDs {
				for i := range vehicles {
					if vehicles[i].ID == vid {
						affectedVehicles = append(affectedVehicles, vehicles[i])
						break
					}
				}
			}
			vehiclesMu.RUnlock()
			if len(affectedVehicles) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "no matching vehicles found"})
				return
			}
			// Fetch existing description so we can append rather than replace.
			getResp, err := jiraRequest("GET", "/rest/api/3/issue/"+issueKey+"?fields=description", nil)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer getResp.Body.Close()
			if getResp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(getResp.Body)
				c.JSON(getResp.StatusCode, gin.H{"error": "failed to fetch existing issue", "detail": string(body)})
				return
			}
			var existing struct {
				Fields struct {
					Description *adfDoc `json:"description"`
				} `json:"fields"`
			}
			if err := json.NewDecoder(getResp.Body).Decode(&existing); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode Jira response"})
				return
			}

			summary, newDoc := buildFleetJiraTicket(req.FilterName, affectedVehicles)

			// Append divider + new period block to existing content.
			var mergedContent []adfNode
			if existing.Fields.Description != nil {
				mergedContent = append(mergedContent, existing.Fields.Description.Content...)
			}
			mergedContent = append(mergedContent, adfNode{Type: "rule"})
			mergedContent = append(mergedContent, newDoc.Content...)

			mergedDoc := adfDoc{Type: "doc", Version: 1, Content: mergedContent}
			payload := map[string]any{
				"fields": map[string]any{
					"summary":     summary,
					"description": mergedDoc,
				},
			}
			resp, err := jiraRequest("PUT", "/rest/api/3/issue/"+issueKey, payload)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				log.Printf("Jira fleet update error %d: %s", resp.StatusCode, body)
				c.JSON(resp.StatusCode, gin.H{"error": "Jira API error", "detail": string(body)})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"key": issueKey,
				"url": fmt.Sprintf("%s/browse/%s", jiraBase, issueKey),
			})
		})

		// PUT /api/jira/update/:key  body: {"vehicle_id":"rog103"}
		api.PUT("/jira/update/:key", func(c *gin.Context) {
			if jiraToken == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jira integration not configured"})
				return
			}
			issueKey := c.Param("key")
			var req struct {
				VehicleID string `json:"vehicle_id"`
			}
			if err := c.BindJSON(&req); err != nil || req.VehicleID == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "vehicle_id required"})
				return
			}
			v := findVehicle(req.VehicleID)
			if v == nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "vehicle not found"})
				return
			}

			// Fetch existing description so we can append rather than replace.
			getResp, err := jiraRequest("GET", "/rest/api/3/issue/"+issueKey+"?fields=description", nil)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer getResp.Body.Close()
			if getResp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(getResp.Body)
				c.JSON(getResp.StatusCode, gin.H{"error": "failed to fetch existing issue", "detail": string(body)})
				return
			}
			var existing struct {
				Fields struct {
					Description *adfDoc `json:"description"`
				} `json:"fields"`
			}
			if err := json.NewDecoder(getResp.Body).Decode(&existing); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode Jira response"})
				return
			}

			summary, newDoc := buildJiraTicket(*v)

			// Merge with history, shrinking how much history we keep until the
			// payload fits under Jira's description size limit.
			var mergedContent []adfNode
			for keep := maxJiraDescriptionSections - 1; ; keep-- {
				mergedContent = nil
				if existing.Fields.Description != nil && keep > 0 {
					mergedContent = trimADFSections(existing.Fields.Description.Content, keep)
				}
				if len(mergedContent) > 0 {
					mergedContent = append(mergedContent, adfNode{Type: "rule"})
				}
				mergedContent = append(mergedContent, newDoc.Content...)
				if keep <= 0 || adfContentBytes(mergedContent) <= maxJiraDescriptionBytes {
					break
				}
			}

			mergedDoc := adfDoc{Type: "doc", Version: 1, Content: mergedContent}
			payload := map[string]any{
				"fields": map[string]any{
					"summary":     summary,
					"description": mergedDoc,
				},
			}
			resp, err := jiraRequest("PUT", "/rest/api/3/issue/"+issueKey, payload)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				log.Printf("Jira update error %d: %s", resp.StatusCode, body)
				c.JSON(resp.StatusCode, gin.H{"error": "Jira API error", "detail": string(body)})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"key": issueKey,
				"url": fmt.Sprintf("%s/browse/%s", jiraBase, issueKey),
			})
		})

		// Triage persistence routes
		api.GET("/triage/current", triageGetCurrentHandler)
		api.GET("/triage/cycles", triageListCyclesHandler)
		api.GET("/triage/cycles/:id", triageGetCycleHandler)
		api.POST("/triage/start", triageStartHandler)
		api.PATCH("/triage/annotations", triageUpsertAnnotationHandler)
		api.PATCH("/triage/action-items", triageUpsertActionItemHandler)
		api.POST("/triage/edit-session/start", triageEditSessionStartHandler)
		api.POST("/triage/edit-session/heartbeat", triageEditSessionHeartbeatHandler)
		api.POST("/triage/edit-session/done", triageEditSessionDoneHandler)
		api.POST("/triage/complete", triageCompleteHandler)
		api.POST("/triage/refresh-tickets", triageRefreshTicketsHandler)
		api.POST("/triage/post-slack", triagePostSlackHandler)
		api.GET("/triage/camera-tickets", triageCameraTicketsHandler)
	}

	if os.Getenv("ENV") == "dev" {
		log.Println("Running in dev mode - frontend should be served by Vite on :3000")
	} else {
		distFS, err := fs.Sub(frontendFS, "frontend/dist")
		if err != nil {
			log.Fatal(err)
		}
		r.NoRoute(func(c *gin.Context) {
			c.FileFromFS(c.Request.URL.Path, http.FS(distFS))
		})
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}
	log.Printf("Server starting on port %s\n", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatal(err)
	}
}
