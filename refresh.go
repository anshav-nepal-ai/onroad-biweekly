package main

// refresh.go — Go port of refresh.py
//
// runRefresh() replaces the .venv/bin/python3 refresh.py subprocess that cannot
// run in the Cloud Run container produced by apps-platform's --no-build deploy.
// It fetches fleet data from Fleetio, ADP SQL, OCI S3, and calibration S3, then
// writes vehicles.json and returns the parsed vehicles slice.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	awssm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	slackpkg "github.com/slack-go/slack"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	rfFleetioBase     = "https://secure.fleetio.com/api/v1"
	rfADPQueryURL     = "https://api.neuron.oci.applied.dev/api/v2/query_sql"
	rfS3Bucket        = "neuron-prod-data-intelligence-frame-gen"
	rfRerunViewBase   = "https://neuron.oci.applied.dev/data_explorer/v2/rerun?version=0.26&rrd="
	rfRerunS3Prefix   = "s3://neuron-prod-data-intelligence-frame-gen/rerun"
	rfCalibBucket     = "neuron-calibrations"
	rfStaleDays       = 7
	rfSnippetsMin     = 2
	rfMaxRecent       = 5
	rfADPWindowDays   = 21
	rfADPBatchSize    = 15
	rfConvStaleHours  = 48
	rfSkipGraceHours  = 48
	rfCalStaleDays    = 30
)

// ---------------------------------------------------------------------------
// Package-level vars
// ---------------------------------------------------------------------------

var (
	rfHTTPClient = &http.Client{Timeout: 130 * time.Second}

	// Nominatim reverse-geocode cache and rate limiter (1 req/sec policy)
	rfGeocodeCache  sync.Map
	rfNominatimMu   sync.Mutex
	rfNominatimLast time.Time

	// Compiled regexes
	rfSelectRe   = regexp.MustCompile(`(?i)\bSELECT\b`)
	rfRunDateRe  = regexp.MustCompile(`_(\d{4})(\d{2})(\d{2})_`)
	rfRunSegRe   = regexp.MustCompile(`^([^-]+_\d{8}_\d{6})-\d+`)
	rfVehicleRe  = regexp.MustCompile(`([A-Za-z]+)\s*-\s*(\d+)`)
	rfNicknameRe = regexp.MustCompile(`\(([^)]+)\)`)
	rfNickValRe  = regexp.MustCompile(`^[a-z][a-z0-9_]+$`)
	rfCalDateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	rfSrcFrameRe = regexp.MustCompile(`source_frame:\s*"([^"]+)"`)
	rfTgtFrameRe = regexp.MustCompile(`target_frame:\s*"([^"]+)"`)
	rfCalTSRe    = regexp.MustCompile(`calibration_metadata\s*\{\s*timestamp\s*\{\s*seconds:\s*(\d+)`)
)

// Sets / maps
var rfInactiveStatuses = map[string]bool{
	"Out of Service": true,
	"Inactive":       true,
	"Build":          true,
	"Verification":   true,
	"Validation":     true,
	"Calibration":    true,
}

var rfRelevantMakes = map[string]bool{
	"Nissan": true, "Ford": true, "Isuzu": true, "Honda": true, "Hyundai": true,
}

var rfRelevantModels = map[string]bool{
	"Rogue": true, "Mustang Mach-E": true, "D-MAX": true, "X-Trail": true, "Civic Hybrid": true, "IONIQ 5": true,
}

var rfLocationNorm = map[string]string{
	"ca": "California", "california": "California",
	"mi": "Michigan", "michigan": "Michigan",
	"fl": "Florida", "florida": "Florida",
	"de": "Germany", "germany": "Germany",
	"japan": "Japan",
}

var rfVehicleOverrides = map[string]string{
	"mce101": "cosmo",
	"mce102": "wanda",
}

var rfCalibCameras = []string{
	"front_center", "front_center_narrow", "front_left", "front_right",
	"rear_center", "rear_left", "rear_right",
}

var rfDataPurposes = []string{
	"Sensor Data Collection", "Sensor Data Collection Extended",
	"Testing: Sensor Data Collection", "Trigger Data Collection",
	"SDS Defense Sensor Data Collection", "Behavior Data Collection",
	"1stage Closed Loop with Hesai Lidar", "Calibration",
	"DCAS Pedestrian Crossing Intersection", "E2E CI Test", "NeuralSim",
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func rfRandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func rfTrunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func rfNormalizeLocation(raw string) string {
	if raw == "" {
		return ""
	}
	if v, ok := rfLocationNorm[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return v
	}
	return strings.TrimSpace(raw)
}

// rfFetchCurrentLocation queries Fleetio's location_entries for a vehicle and returns
// "City, State" for US vehicles or "Country" for others. Returns "" if unavailable.
// It first tries pre-geocoded address_components; if absent, falls back to raw
// geolocation coordinates via rfReverseGeocode.
func rfFetchCurrentLocation(numericID int, apiToken, accountToken string) string {
	url := fmt.Sprintf("%s/vehicles/%d/location_entries?per_page=5", rfFleetioBase, numericID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Token "+apiToken)
	req.Header.Set("Account-Token", accountToken)

	resp, err := rfHTTPClient.Do(req)
	if err != nil {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}

	var data struct {
		Records []map[string]interface{} `json:"records"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return ""
	}

	for _, rec := range data.Records {
		// Prefer pre-geocoded address_components (no extra HTTP call needed).
		if ac, ok := rec["address_components"].(map[string]interface{}); ok && len(ac) > 0 {
			city := strVal(ac, "city")
			region := strVal(ac, "region")
			country := strVal(ac, "country")
			countryShort := strVal(ac, "country_short")
			if city != "" || country != "" {
				if countryShort == "US" {
					if city != "" && region != "" {
						return city + ", " + region
					}
					if region != "" {
						return region
					}
				}
				if country != "" {
					return country
				}
			}
		}

		// Fall back to raw geolocation coordinates when address_components is absent.
		if geo, ok := rec["geolocation"].(map[string]interface{}); ok {
			lat, _ := geo["latitude"].(float64)
			lng, _ := geo["longitude"].(float64)
			if lat != 0 || lng != 0 {
				if loc := rfReverseGeocode(lat, lng); loc != "" {
					return loc
				}
			}
		}
	}
	return ""
}

// rfReverseGeocode converts coordinates to "City, State" (US) or "Country" (elsewhere)
// using the Nominatim API. Results are cached by rounded coordinate to avoid duplicate
// calls for vehicles at the same site. Nominatim's 1 req/sec policy is enforced globally.
func rfReverseGeocode(lat, lng float64) string {
	key := fmt.Sprintf("%.2f,%.2f", lat, lng)
	if v, ok := rfGeocodeCache.Load(key); ok {
		return v.(string)
	}

	// Serialize calls and enforce 1.1s gap to respect Nominatim's rate limit.
	rfNominatimMu.Lock()
	if elapsed := time.Since(rfNominatimLast); elapsed < 1100*time.Millisecond {
		time.Sleep(1100*time.Millisecond - elapsed)
	}
	rfNominatimLast = time.Now()
	rfNominatimMu.Unlock()

	geoURL := fmt.Sprintf("https://nominatim.openstreetmap.org/reverse?lat=%.6f&lon=%.6f&format=json", lat, lng)
	req, err := http.NewRequest("GET", geoURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "onroad-biweekly-fleet-dashboard/1.0")

	resp, err := rfHTTPClient.Do(req)
	if err != nil {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}

	var result struct {
		Address struct {
			City        string `json:"city"`
			Town        string `json:"town"`
			Village     string `json:"village"`
			State       string `json:"state"`
			Country     string `json:"country"`
			CountryCode string `json:"country_code"`
		} `json:"address"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}

	city := result.Address.City
	if city == "" {
		city = result.Address.Town
	}
	if city == "" {
		city = result.Address.Village
	}

	var loc string
	if strings.EqualFold(result.Address.CountryCode, "us") {
		if city != "" && result.Address.State != "" {
			loc = city + ", " + result.Address.State
		} else if result.Address.State != "" {
			loc = result.Address.State
		}
	} else if result.Address.Country != "" {
		loc = result.Address.Country
	}

	if loc != "" {
		rfGeocodeCache.Store(key, loc)
	}
	return loc
}

func rfVehicleProject(labels []string) string {
	gen := "Gen-1"
	for _, l := range labels {
		if l == "Gen2" {
			gen = "Gen-2"
			break
		}
	}
	for _, l := range labels {
		if l == "Oasis" {
			return "Oasis " + gen
		}
		if l == "robotaxi" {
			return "Robotaxi " + gen
		}
	}
	return "Neuron " + gen
}

func rfExtractNickname(rawName string) string {
	m := rfNicknameRe.FindStringSubmatch(rawName)
	if m == nil {
		return ""
	}
	candidate := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(m[1]), " ", "_"))
	if rfNickValRe.MatchString(candidate) {
		return candidate
	}
	return ""
}

// rfRunFilenameToTime parses a date from a run filename like rog101_20260612_131514.
// Returns the zero Time if no date found.
func rfRunFilenameToTime(filename string) time.Time {
	m := rfRunDateRe.FindStringSubmatch(filename)
	if m == nil {
		return time.Time{}
	}
	t, err := time.Parse("20060102", m[1]+m[2]+m[3])
	if err != nil {
		return time.Time{}
	}
	return t
}

func rfToday() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour)
}

func rfDateStr(t time.Time) string {
	return t.Format("2006-01-02")
}

// strVal extracts a string from map[string]interface{}, returning "" if absent or non-string.
func strVal(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

// intVal extracts an int or float64 from map[string]interface{}, returning 0 if absent.
func intVal(m map[string]interface{}, key string) int {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// ---------------------------------------------------------------------------
// Fleetio
// ---------------------------------------------------------------------------

// rfFleetVehicle is an intermediate representation while fetching from Fleetio.
type rfFleetVehicle struct {
	vehicleID      string
	filenamePrefix string
	status         string
	project        string
	location       string
	fleetioNumID   int
}

// rfGetFleetioVehicles returns (active, inactive) vehicles matching the fleet.
func rfGetFleetioVehicles() (active, inactive []rfFleetVehicle, err error) {
	accountToken := os.Getenv("FLEETIO_ACCOUNT_TOKEN")
	if accountToken == "" {
		accountToken = "a0294e2c81"
	}
	apiToken := os.Getenv("FLEETIO_API_TOKEN")
	if apiToken == "" {
		apiToken = "f9c8bdf9e92d563ba117924560478701ad17059f"
	}

	// Collect all matching vehicles across pages before enriching locations.
	var entries []rfFleetVehicle
	cursor := ""
	for {
		reqURL := rfFleetioBase + "/vehicles?per_page=50"
		if cursor != "" {
			reqURL += "&start_cursor=" + cursor
		}
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Token "+apiToken)
		req.Header.Set("Account-Token", accountToken)

		resp, err := rfHTTPClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, nil, fmt.Errorf("Fleetio %d: %s", resp.StatusCode, rfTrunc(string(body), 300))
		}

		var data struct {
			Records    []map[string]interface{} `json:"records"`
			NextCursor string                   `json:"next_cursor"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, nil, err
		}
		if len(data.Records) == 0 {
			break
		}

		for _, v := range data.Records {
			make_ := strVal(v, "make")
			model := strVal(v, "model")
			if !rfRelevantMakes[make_] || !rfRelevantModels[model] {
				continue
			}

			// Determine status
			status := strVal(v, "vehicle_status_name")
			if status == "" {
				if vs, ok := v["vehicle_status"].(map[string]interface{}); ok {
					status = strVal(vs, "name")
				}
			}
			if status == "" {
				status = strVal(v, "status")
			}

			rawName := strings.TrimSpace(strVal(v, "name"))
			var vid string
			if m := rfVehicleRe.FindStringSubmatch(rawName); m != nil {
				vid = strings.ToLower(m[1]) + m[2]
			} else {
				fields := strings.Fields(rawName)
				if len(fields) > 0 {
					vid = strings.ToLower(fields[0])
				}
			}

			nickname := rfVehicleOverrides[vid]
			if nickname == "" {
				nickname = rfExtractNickname(rawName)
			}
			prefix := nickname
			if prefix == "" {
				prefix = vid
			}

			// Labels
			var labelNames []string
			if labels, ok := v["labels"].([]interface{}); ok {
				for _, l := range labels {
					if lm, ok := l.(map[string]interface{}); ok {
						labelNames = append(labelNames, strVal(lm, "name"))
					}
				}
			}

			entries = append(entries, rfFleetVehicle{
				vehicleID:      vid,
				filenamePrefix: prefix,
				status:         status,
				project:        rfVehicleProject(labelNames),
				location:       rfNormalizeLocation(strVal(v, "registration_state")),
				fleetioNumID:   intVal(v, "id"),
			})
		}

		cursor = data.NextCursor
		if cursor == "" {
			break
		}
	}

	// Enrich each vehicle's location from Fleetio GPS data concurrently.
	// Fall back to the registration_state value already set if no GPS data found.
	var mu sync.Mutex
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i := range entries {
		if entries[i].fleetioNumID == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			loc := rfFetchCurrentLocation(entries[i].fleetioNumID, apiToken, accountToken)
			if loc != "" {
				mu.Lock()
				entries[i].location = loc
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	for _, e := range entries {
		if rfInactiveStatuses[e.status] {
			inactive = append(inactive, e)
		} else {
			active = append(active, e)
		}
	}
	return active, inactive, nil
}

// ---------------------------------------------------------------------------
// URSA token
// ---------------------------------------------------------------------------

func rfGetUrsaToken(ctx context.Context) (string, error) {
	clientID := os.Getenv("NEURON_CLIENT_ID")
	clientSecret := os.Getenv("NEURON_CLIENT_SECRET")

	if clientID == "" || clientSecret == "" {
		// Local dev fallback: fetch from AWS Secrets Manager via named profile
		profileName := os.Getenv("AWS_PROFILE_NEURON")
		if profileName == "" {
			profileName = "neuron"
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithSharedConfigProfile(profileName),
			awsconfig.WithRegion("us-west-2"),
		)
		if err != nil {
			return "", fmt.Errorf("load neuron AWS config: %w", err)
		}
		sm := awssm.NewFromConfig(cfg)
		result, err := sm.GetSecretValue(ctx, &awssm.GetSecretValueInput{
			SecretId: aws.String("neuron-machine-auth"),
		})
		if err != nil {
			return "", fmt.Errorf("get neuron-machine-auth secret: %w", err)
		}
		var creds struct {
			ClientID     string `json:"CLIENT_ID"`
			ClientSecret string `json:"CLIENT_SECRET"`
		}
		if err := json.Unmarshal([]byte(aws.ToString(result.SecretString)), &creds); err != nil {
			return "", fmt.Errorf("parse neuron secret: %w", err)
		}
		clientID = creds.ClientID
		clientSecret = creds.ClientSecret
	}

	payload, _ := json.Marshal(map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
	})
	resp, err := rfHTTPClient.Post(
		"https://accounts.applied.co/api/machineCredential/get",
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("URSA token %d: %s", resp.StatusCode, rfTrunc(string(body), 300))
	}
	var tr struct {
		Token string `json:"machine_auth_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	return tr.Token, nil
}

// ---------------------------------------------------------------------------
// ADP SQL
// ---------------------------------------------------------------------------

func rfQueryADP(ctx context.Context, sql, token string, limit int) ([]map[string]interface{}, error) {
	if limit == 0 {
		limit = 1000
	}
	activeSql := sql
	for attempt := 0; attempt < 3; attempt++ {
		type adpOpts struct {
			DataSource string `json:"dataSource"`
			Limit      int    `json:"limit"`
		}
		type adpReq struct {
			SQLQuery        string  `json:"sqlQuery"`
			SQLQueryOptions adpOpts `json:"sqlQueryOptions"`
		}
		reqBody, _ := json.Marshal(adpReq{
			SQLQuery:        activeSql,
			SQLQueryOptions: adpOpts{DataSource: "ADP_QUERY_ENGINE", Limit: limit},
		})

		httpReq, err := http.NewRequestWithContext(ctx, "POST", rfADPQueryURL, bytes.NewReader(reqBody))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := rfHTTPClient.Do(httpReq)
		if err != nil {
			return nil, err
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			bodyStr := string(respBody)
			isAbandoned := resp.StatusCode == 400 && strings.Contains(bodyStr, "abandoned by the client")
			isRateLimited := resp.StatusCode == 429
			if (isAbandoned || isRateLimited) && attempt < 2 {
				if isRateLimited {
					wait := 30
					if ra := resp.Header.Get("Retry-After"); ra != "" {
						if w, e := strconv.Atoi(ra); e == nil {
							wait = w
						}
					}
					time.Sleep(time.Duration(wait) * time.Second)
				} else {
					nonce := rfRandomHex(16)
					loc := rfSelectRe.FindStringIndex(sql)
					if loc != nil {
						activeSql = sql[:loc[0]] + "SELECT '" + nonce + "' AS _nonce," + sql[loc[1]:]
					}
					time.Sleep(time.Duration(5*(attempt+1)) * time.Second)
				}
				continue
			}
			return nil, fmt.Errorf("ADP SQL %d: %s", resp.StatusCode, rfTrunc(bodyStr, 500))
		}

		var apiResp struct {
			Message    string `json:"message"`
			ResultJSON string `json:"resultJson"`
		}
		if err := json.Unmarshal(respBody, &apiResp); err != nil {
			return nil, fmt.Errorf("ADP JSON parse: %w", err)
		}
		if apiResp.Message != "" {
			return nil, fmt.Errorf("ADP SQL error: %s", apiResp.Message)
		}
		rj := apiResp.ResultJSON
		if rj == "" {
			rj = "[]"
		}
		var rows []map[string]interface{}
		if err := json.Unmarshal([]byte(rj), &rows); err != nil {
			return nil, fmt.Errorf("ADP resultJson parse: %w", err)
		}
		return rows, nil
	}
	return nil, fmt.Errorf("rfQueryADP: exhausted retries")
}

// ---------------------------------------------------------------------------
// ADP SQL queries
// ---------------------------------------------------------------------------

type rfRunInfo struct {
	runFilename string
	runUUID     string
}

// rfGetLatestRuns returns {filenamePrefix: [{runFilename, runUUID}, ...]} newest-first.
func rfGetLatestRuns(ctx context.Context, prefixes []string, token string) (map[string][]rfRunInfo, error) {
	dtFrom := rfDateStr(rfToday().AddDate(0, 0, -rfADPWindowDays))
	dtTo := rfDateStr(rfToday())

	result := map[string][]rfRunInfo{}
	for i := 0; i < len(prefixes); i += rfADPBatchSize {
		end := i + rfADPBatchSize
		if end > len(prefixes) {
			end = len(prefixes)
		}
		batch := prefixes[i:end]

		parts := make([]string, len(batch))
		for j, p := range batch {
			parts[j] = "customentity.segment_info.segment_id LIKE '" + p + "_%'"
		}
		conditions := strings.Join(parts, " OR ")
		sql := fmt.Sprintf(
			"SELECT run_uuid, MAX(customentity.segment_info.segment_id) AS latest_seg "+
				"FROM hudi_hms.ursa_lake.mlds_unified_feature_metadata "+
				"WHERE dt >= '%s' AND dt <= '%s' "+
				"AND (%s) "+
				"GROUP BY run_uuid "+
				"ORDER BY latest_seg DESC",
			dtFrom, dtTo, conditions,
		)

		rows, err := rfQueryADP(ctx, sql, token, 1000)
		if err != nil {
			return nil, err
		}

		for _, row := range rows {
			seg := strVal(row, "latest_seg")
			m := rfRunSegRe.FindStringSubmatch(seg)
			if m == nil {
				continue
			}
			runFilename := m[1]
			var prefix string
			for _, p := range batch {
				if strings.HasPrefix(runFilename, p+"_") {
					prefix = p
					break
				}
			}
			if prefix == "" {
				continue
			}
			result[prefix] = append(result[prefix], rfRunInfo{
				runFilename: runFilename,
				runUUID:     strVal(row, "run_uuid"),
			})
		}
	}

	for k := range result {
		runs := result[k]
		sort.Slice(runs, func(i, j int) bool {
			return runs[i].runFilename > runs[j].runFilename
		})
		result[k] = runs
	}
	return result, nil
}

// rfGetSkippedRuns returns {vehicleName: [{...}]} for runs that converted but never reached ORDC.
func rfGetSkippedRuns(ctx context.Context, prefixes []string, token string) (map[string][]SkippedRun, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	dtFrom := rfDateStr(rfToday().AddDate(0, 0, -rfStaleDays))
	prefixList := "'" + strings.Join(prefixes, "', '") + "'"
	purposeList := "'" + strings.Join(rfDataPurposes, "', '") + "'"

	sql := fmt.Sprintf(
		"WITH base AS ("+
			"  SELECT CAST(d.uuid AS VARCHAR) AS run_uuid, d.vehicle_name, "+
			"         d.log_collected_at, d.duration_s "+
			"  FROM ursa_log_management.public.duration_in_seconds_view d "+
			"  JOIN ursa_log_management.public.log_conversions c ON c.source_run_uuid = d.uuid "+
			"  WHERE c.conversion_status = 2 "+
			"  AND d.vehicle_name IN (%s) "+
			"  AND DATE(d.log_collected_at) >= DATE('%s') "+
			"  AND d.log_collected_at <= current_timestamp - INTERVAL '%d' HOUR "+
			"  AND d.purpose IN (%s) "+
			"), "+
			"ordc_runs AS ("+
			"  SELECT DISTINCT run_uuid "+
			"  FROM hudi_hms.ursa_metric.ordc_dashboard_runs_v2 "+
			"  WHERE dt >= '%s' "+
			"  AND vehicle_name IN (%s) "+
			") "+
			"SELECT b.run_uuid, b.vehicle_name, "+
			"  CAST(b.log_collected_at AS VARCHAR) AS log_collected_at, b.duration_s "+
			"FROM base b "+
			"LEFT JOIN ordc_runs o ON o.run_uuid = b.run_uuid "+
			"WHERE o.run_uuid IS NULL "+
			"ORDER BY b.vehicle_name, b.log_collected_at DESC",
		prefixList, dtFrom, rfSkipGraceHours, purposeList, dtFrom, prefixList,
	)

	rows, err := rfQueryADP(ctx, sql, token, 500)
	if err != nil {
		return nil, err
	}

	byVehicle := map[string][]SkippedRun{}
	for _, row := range rows {
		vname := strVal(row, "vehicle_name")
		tsStr := strVal(row, "log_collected_at")
		var driveID, runDate string
		if dt, ok := rfParseTimestamp(tsStr); ok {
			driveID = fmt.Sprintf("%s_%s", vname, dt.Format("20060102_150405"))
			runDate = dt.Format("2006-01-02")
		}
		if driveID == "" {
			driveID = strVal(row, "run_uuid")
		}
		durMin := intVal(row, "duration_s") / 60
		if durMin == 0 {
			continue
		}
		byVehicle[vname] = append(byVehicle[vname], SkippedRun{
			RunUUID:         strVal(row, "run_uuid"),
			DriveID:         driveID,
			RunDate:         runDate,
			DurationMinutes: durMin,
			Reason:          "converted but never reached ORDC — exact skip reason isn't queryable via ADP SQL",
		})
	}
	return byVehicle, nil
}

// rfGetConversionIssues returns {vehicleName: [{...}]} for drives with failed/stuck conversion.
func rfGetConversionIssues(ctx context.Context, prefixes []string, token string) (map[string][]ConversionIssue, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	dtFrom := rfDateStr(rfToday().AddDate(0, 0, -rfStaleDays))
	prefixList := "'" + strings.Join(prefixes, "', '") + "'"
	purposeList := "'" + strings.Join(rfDataPurposes, "', '") + "'"

	sql := fmt.Sprintf(
		"SELECT CAST(d.uuid AS VARCHAR) AS run_uuid, d.vehicle_name, "+
			"  CAST(d.log_collected_at AS VARCHAR) AS log_collected_at, d.duration_s, "+
			"  c.conversion_status "+
			"FROM ursa_log_management.public.duration_in_seconds_view d "+
			"LEFT JOIN ursa_log_management.public.log_conversions c ON c.source_run_uuid = d.uuid "+
			"WHERE d.vehicle_name IN (%s) "+
			"AND DATE(d.log_collected_at) >= DATE('%s') "+
			"AND d.log_collected_at <= current_timestamp - INTERVAL '%d' HOUR "+
			"AND d.purpose IN (%s) "+
			"AND d.duration_s > 0 "+
			"AND (c.source_run_uuid IS NULL OR c.conversion_status IN (1, 3)) "+
			"ORDER BY d.vehicle_name, d.log_collected_at DESC",
		prefixList, dtFrom, rfConvStaleHours, purposeList,
	)

	rows, err := rfQueryADP(ctx, sql, token, 500)
	if err != nil {
		return nil, err
	}

	// Dedupe by run_uuid, keeping worst status (failed=3 > pending=1)
	type dedupeEntry struct {
		row        map[string]interface{}
		statusRank int // 1=failed(3), 0=pending(1)/none
	}
	byRunUUID := map[string]dedupeEntry{}
	for _, row := range rows {
		uuid := strVal(row, "run_uuid")
		status := intVal(row, "conversion_status")
		rank := 0
		if status == 3 {
			rank = 1
		}
		if existing, ok := byRunUUID[uuid]; ok && existing.statusRank >= rank {
			continue
		}
		byRunUUID[uuid] = dedupeEntry{row: row, statusRank: rank}
	}

	byVehicle := map[string][]ConversionIssue{}
	for _, entry := range byRunUUID {
		row := entry.row
		vname := strVal(row, "vehicle_name")
		tsStr := strVal(row, "log_collected_at")
		var driveID, runDate string
		if dt, ok := rfParseTimestamp(tsStr); ok {
			driveID = fmt.Sprintf("%s_%s", vname, dt.Format("20060102_150405"))
			runDate = dt.Format("2006-01-02")
		}
		if driveID == "" {
			driveID = strVal(row, "run_uuid")
		}
		durMin := intVal(row, "duration_s") / 60
		if durMin == 0 {
			continue
		}
		status := intVal(row, "conversion_status")
		reason := "never submitted for log conversion"
		switch status {
		case 1:
			reason = fmt.Sprintf("log conversion still pending after %dh", rfConvStaleHours)
		case 3:
			reason = "log conversion failed"
		}
		byVehicle[vname] = append(byVehicle[vname], ConversionIssue{
			RunUUID:         strVal(row, "run_uuid"),
			DriveID:         driveID,
			RunDate:         runDate,
			DurationMinutes: durMin,
			Reason:          reason,
		})
	}
	return byVehicle, nil
}

// rfGetZeroSegmentRuns returns runs where frame gen ran but produced 0 usable seconds.
func rfGetZeroSegmentRuns(ctx context.Context, prefixes []string, token string) (map[string][]ZeroSegmentRun, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	dtFrom := rfDateStr(rfToday().AddDate(0, 0, -rfStaleDays))
	prefixList := "'" + strings.Join(prefixes, "', '") + "'"

	sql := fmt.Sprintf(
		"SELECT r.run_uuid, r.vehicle_name, r.custom_id AS run_id, r.recording_seconds, "+
			"f.filter_name, SUM(f.fail_seconds) AS total_fail_seconds "+
			"FROM hudi_hms.ursa_metric.ordc_dashboard_runs_v2 r "+
			"LEFT JOIN hudi_hms.ursa_metric.ordc_dashboard_filter_metrics_v2 f "+
			"  ON r.run_uuid = f.run_uuid AND f.dt >= '%s' AND f.fail_seconds > 0 "+
			"WHERE r.dt >= '%s' "+
			"AND r.vehicle_name IN (%s) "+
			"AND COALESCE(r.usable_seconds, 0) = 0 "+
			"AND COALESCE(r.recording_seconds, 0) > 0 "+
			"GROUP BY r.run_uuid, r.vehicle_name, r.custom_id, r.recording_seconds, f.filter_name "+
			"ORDER BY r.vehicle_name, r.custom_id DESC, total_fail_seconds DESC",
		dtFrom, dtFrom, prefixList,
	)

	rows, err := rfQueryADP(ctx, sql, token, 5000)
	if err != nil {
		return nil, err
	}

	type runKey struct{ vname, runID string }
	type runData struct {
		runUUID    string
		runID      string
		recSeconds int
		filters    []FilterEntry
	}
	byVehicleRun := map[runKey]*runData{}
	runOrder := []runKey{} // preserve insertion order for vehicle/run keys

	for _, row := range rows {
		vname := strVal(row, "vehicle_name")
		runID := strVal(row, "run_id")
		if runID == "" {
			runID = strVal(row, "run_uuid")
		}
		key := runKey{vname, runID}
		if _, exists := byVehicleRun[key]; !exists {
			byVehicleRun[key] = &runData{
				runUUID:    strVal(row, "run_uuid"),
				runID:      runID,
				recSeconds: intVal(row, "recording_seconds"),
			}
			runOrder = append(runOrder, key)
		}
		fname := strVal(row, "filter_name")
		if fname != "" {
			recSec := byVehicleRun[key].recSeconds
			failSec := intVal(row, "total_fail_seconds")
			failPct := 0.0
			if recSec > 0 {
				failPct = float64(failSec) / float64(recSec) * 100
			}
			byVehicleRun[key].filters = append(byVehicleRun[key].filters, FilterEntry{
				FilterName:  fname,
				FailSeconds: failSec,
				FailPct:     rfRound1(failPct),
			})
		}
	}

	byVehicle := map[string][]ZeroSegmentRun{}
	for _, key := range runOrder {
		rd := byVehicleRun[key]
		durMin := rd.recSeconds / 60
		if durMin == 0 {
			continue
		}
		runID := rd.runID
		runDate := ""
		if m := rfRunDateRe.FindStringSubmatch(runID); m != nil {
			runDate = fmt.Sprintf("%s-%s-%s", m[1], m[2], m[3])
		}
		// Sort filters by fail_seconds descending
		filters := append([]FilterEntry(nil), rd.filters...)
		sort.Slice(filters, func(i, j int) bool {
			return filters[i].FailSeconds > filters[j].FailSeconds
		})
		byVehicle[key.vname] = append(byVehicle[key.vname], ZeroSegmentRun{
			RunUUID:         rd.runUUID,
			RunID:           runID,
			RunDate:         runDate,
			DurationMinutes: durMin,
			Filters:         filters,
		})
	}
	return byVehicle, nil
}

// rfGetQualityRuns returns {vehicleName: [{runID, runUUID, runMinutes, filters}]} for all runs in last 7 days.
func rfGetQualityRuns(ctx context.Context, prefixes []string, token string) (map[string][]QualityRun, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	dtFrom := rfDateStr(rfToday().AddDate(0, 0, -rfStaleDays))
	prefixList := "'" + strings.Join(prefixes, "', '") + "'"

	sql := fmt.Sprintf(
		"SELECT r.vehicle_name, r.custom_id AS run_id, r.run_uuid, r.recording_seconds, "+
			"f.filter_name, SUM(f.fail_seconds) AS total_fail_seconds "+
			"FROM hudi_hms.ursa_metric.ordc_dashboard_runs_v2 r "+
			"JOIN hudi_hms.ursa_metric.ordc_dashboard_filter_metrics_v2 f ON r.run_uuid = f.run_uuid "+
			"WHERE r.dt >= '%s' AND f.dt >= '%s' "+
			"AND r.vehicle_name IN (%s) "+
			"AND f.fail_seconds > 0 "+
			"AND f.filter_name != 'location_in_garage' "+
			"AND f.filter_name != 'record_pause' "+
			"AND f.filter_name NOT LIKE '%%slam_quality%%' "+
			"AND f.filter_name NOT LIKE '%%geofence%%' "+
			"GROUP BY r.vehicle_name, r.custom_id, r.run_uuid, r.recording_seconds, f.filter_name "+
			"ORDER BY r.vehicle_name, r.custom_id DESC, total_fail_seconds DESC",
		dtFrom, dtFrom, prefixList,
	)

	rows, err := rfQueryADP(ctx, sql, token, 10000)
	if err != nil {
		return nil, err
	}

	type runKey struct{ vname, runID string }
	type runData struct {
		runUUID    string
		runID      string
		recSeconds int
		filters    []FilterEntry
	}
	byVehicleRun := map[runKey]*runData{}
	runOrder := []runKey{}

	for _, row := range rows {
		vname := strVal(row, "vehicle_name")
		runID := strVal(row, "run_id")
		key := runKey{vname, runID}
		if _, exists := byVehicleRun[key]; !exists {
			byVehicleRun[key] = &runData{
				runUUID:    strVal(row, "run_uuid"),
				runID:      runID,
				recSeconds: intVal(row, "recording_seconds"),
			}
			runOrder = append(runOrder, key)
		}
		recSec := byVehicleRun[key].recSeconds
		failSec := intVal(row, "total_fail_seconds")
		failPct := 0.0
		if recSec > 0 {
			failPct = float64(failSec) / float64(recSec) * 100
		}
		byVehicleRun[key].filters = append(byVehicleRun[key].filters, FilterEntry{
			FilterName:  strVal(row, "filter_name"),
			FailSeconds: failSec,
			FailPct:     rfRound1(failPct),
		})
	}

	byVehicle := map[string][]QualityRun{}
	for _, key := range runOrder {
		rd := byVehicleRun[key]
		runMin := 0
		if rd.recSeconds > 0 {
			runMin = rd.recSeconds / 60
		}
		byVehicle[key.vname] = append(byVehicle[key.vname], QualityRun{
			RunID:      rd.runID,
			RunUUID:    rd.runUUID,
			RunMinutes: runMin,
			Filters:    rd.filters,
		})
	}

	// Each vehicle's runs are already in run_id DESC order from the SQL ordering
	return byVehicle, nil
}

func rfRound1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

// rfParseTimestamp parses a timestamp string returned by Trino's CAST(ts AS VARCHAR).
// Trino produces "YYYY-MM-DD HH:MM:SS.nnn" (space-separated); we also accept the
// ISO-8601 "T" form so local / test data works too.
func rfParseTimestamp(tsStr string) (time.Time, bool) {
	if len(tsStr) < 19 {
		return time.Time{}, false
	}
	s := tsStr[:19]
	// Normalise the separator to 'T' then parse once.
	if s[10] == ' ' {
		s = s[:10] + "T" + s[11:]
	}
	t, err := time.Parse("2006-01-02T15:04:05", s)
	return t, err == nil
}

// ---------------------------------------------------------------------------
// S3
// ---------------------------------------------------------------------------

func rfNewS3Client(ctx context.Context) (*s3.Client, error) {
	accessKey := os.Getenv("OCI_S3_ACCESS_KEY_ID")
	secretKey := os.Getenv("OCI_S3_SECRET_ACCESS_KEY")
	endpointURL := os.Getenv("OCI_S3_ENDPOINT_URL")

	if accessKey != "" && secretKey != "" {
		cfg := aws.Config{
			Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
			Region:      "us-phoenix-1",
		}
		opts := []func(*s3.Options){}
		if endpointURL != "" {
			opts = append(opts, func(o *s3.Options) {
				o.BaseEndpoint = aws.String(endpointURL)
				o.UsePathStyle = true
			})
		}
		return s3.NewFromConfig(cfg, opts...), nil
	}

	// Local dev: use named AWS profile
	profileName := os.Getenv("AWS_PROFILE_S3")
	if profileName == "" {
		profileName = "oci.phx"
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithSharedConfigProfile(profileName))
	if err != nil {
		return nil, fmt.Errorf("load S3 AWS config (profile=%s): %w", profileName, err)
	}
	return s3.NewFromConfig(cfg), nil
}

// rfListRRDFiles lists .rrd files in s3://rfS3Bucket/rerun/<uuid>/.
func rfListRRDFiles(ctx context.Context, s3Client *s3.Client, runUUID string) []string {
	prefix := "rerun/" + runUUID + "/"
	result, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(rfS3Bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil
	}
	var files []string
	for _, obj := range result.Contents {
		key := aws.ToString(obj.Key)
		if strings.HasSuffix(key, ".rrd") {
			parts := strings.Split(key, "/")
			files = append(files, parts[len(parts)-1])
		}
	}
	return files
}

// ---------------------------------------------------------------------------
// Calibration
// ---------------------------------------------------------------------------

// rfLatestCalibDate returns the latest YYYY-MM-DD calibration folder for a vehicle prefix.
func rfLatestCalibDate(ctx context.Context, s3Client *s3.Client, vehiclePrefix string) string {
	paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(rfCalibBucket),
		Prefix:    aws.String(vehiclePrefix + "/"),
		Delimiter: aws.String("/"),
	})
	var dates []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			break
		}
		for _, cp := range page.CommonPrefixes {
			p := strings.TrimSuffix(aws.ToString(cp.Prefix), "/")
			parts := strings.Split(p, "/")
			if len(parts) == 2 && rfCalDateRe.MatchString(parts[1]) {
				dates = append(dates, parts[1])
			}
		}
	}
	if len(dates) == 0 {
		return ""
	}
	sort.Strings(dates)
	return dates[len(dates)-1]
}

// rfParseCameraCalibration parses a -transforms.txtpb file, returning {cameraName: unixSeconds}.
func rfParseCameraCalibration(text string) map[string]int64 {
	result := map[string]int64{}
	camSet := map[string]bool{}
	for _, c := range rfCalibCameras {
		camSet[c] = true
	}
	blocks := strings.Split(text, "static_transforms {")
	for _, block := range blocks[1:] {
		sf := rfSrcFrameRe.FindStringSubmatch(block)
		tf := rfTgtFrameRe.FindStringSubmatch(block)
		ts := rfCalTSRe.FindStringSubmatch(block)
		if sf == nil || tf == nil || ts == nil {
			continue
		}
		if !camSet[sf[1]] || tf[1] != "hesai_pandar_lidar" {
			continue
		}
		secs, err := strconv.ParseInt(ts[1], 10, 64)
		if err != nil {
			continue
		}
		result[sf[1]] = secs
	}
	return result
}

// rfCalibrationForVehicle fetches and parses calibration data for one vehicle.
func rfCalibrationForVehicle(ctx context.Context, s3Client *s3.Client, prefix string) []CameraCalibration {
	latestDate := rfLatestCalibDate(ctx, s3Client, prefix)
	if latestDate == "" {
		return nil
	}

	// Try both directory layouts
	candidateKeys := []string{
		prefix + "/" + latestDate + "/results/" + latestDate + "-transforms.txtpb",
		prefix + "/" + latestDate + "/" + latestDate + "-transforms.txtpb",
	}
	var text string
	for _, key := range candidateKeys {
		obj, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(rfCalibBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(obj.Body)
		obj.Body.Close()
		text = string(data)
		break
	}
	if text == "" {
		return nil
	}

	camTimestamps := rfParseCameraCalibration(text)
	today := rfToday()
	var entries []CameraCalibration
	for _, cam := range rfCalibCameras {
		secs, ok := camTimestamps[cam]
		if !ok {
			continue
		}
		calDate := time.Unix(secs, 0).UTC().Truncate(24 * time.Hour)
		daysSince := int(today.Sub(calDate).Hours() / 24)
		entries = append(entries, CameraCalibration{
			Camera:         cam,
			LastCalibrated: rfDateStr(calDate),
			DaysSince:      daysSince,
			Stale:          daysSince > rfCalStaleDays,
		})
	}
	// Sort most-overdue first
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].DaysSince > entries[j].DaysSince
	})
	return entries
}

// rfGetCalibrationStatus fetches calibration for all prefixes in parallel.
func rfGetCalibrationStatus(ctx context.Context, s3Client *s3.Client, prefixes []string) map[string][]CameraCalibration {
	type result struct {
		prefix  string
		entries []CameraCalibration
	}
	ch := make(chan result, len(prefixes))
	sem := make(chan struct{}, 10) // max 10 concurrent
	var wg sync.WaitGroup
	for _, p := range prefixes {
		wg.Add(1)
		go func(prefix string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			entries := rfCalibrationForVehicle(ctx, s3Client, prefix)
			ch <- result{prefix, entries}
		}(p)
	}
	wg.Wait()
	close(ch)

	out := map[string][]CameraCalibration{}
	for r := range ch {
		if len(r.entries) > 0 {
			out[r.prefix] = r.entries
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Per-vehicle refresh
// ---------------------------------------------------------------------------

func rfRefreshVehicle(ctx context.Context, s3Client *s3.Client, fv rfFleetVehicle, candidates []rfRunInfo) Vehicle {
	vid := fv.vehicleID
	project := fv.project
	location := fv.location

	if len(candidates) == 0 {
		return Vehicle{
			ID:           vid,
			Project:      project,
			Location:     location,
			Run:          "",
			CamGen:       "n/a",
			Snippets:     []Snippet{},
			RecentRuns:   []RecentRun{},
			Stale:        true,
			StaleReason:  "no run found in ADP",
			Inactive:     false,
			QualityRuns:  []QualityRun{},
			Calibrations: []CameraCalibration{},
		}
	}

	today := rfToday()
	var recentRuns []RecentRun
	var firstFresh *RecentRun
	lastReason := "no run found in ADP"

	for _, ri := range candidates {
		if len(recentRuns) >= rfMaxRecent {
			break
		}
		runDate := rfRunFilenameToTime(ri.runFilename)
		ageDays := 0
		if !runDate.IsZero() {
			ageDays = int(today.Sub(runDate).Hours() / 24)
		}

		rrdFiles := rfListRRDFiles(ctx, s3Client, ri.runUUID)
		snippets := []Snippet{}
		for i, f := range rrdFiles {
			if i >= 3 {
				break
			}
			snippets = append(snippets, Snippet{
				Label: fmt.Sprintf("Snippet %d", i+1),
				URL:   rfRerunViewBase + rfRerunS3Prefix + "/" + ri.runUUID + "/" + f,
			})
		}
		rr := RecentRun{
			Run:      ri.runFilename,
			RunUUID:  ri.runUUID,
			RunDate:  rfDateStr(runDate),
			Snippets: snippets,
		}
		recentRuns = append(recentRuns, rr)

		if ageDays > rfStaleDays {
			lastReason = fmt.Sprintf("last run %dd ago", ageDays)
		} else if len(rrdFiles) >= rfSnippetsMin {
			if firstFresh == nil {
				cp := rr
				firstFresh = &cp
			}
		} else {
			lastReason = fmt.Sprintf("only %d snippet(s) rendered", len(rrdFiles))
		}
	}

	if firstFresh != nil {
		return Vehicle{
			ID:           vid,
			Project:      project,
			Location:     location,
			Run:          firstFresh.Run,
			RunUUID:      firstFresh.RunUUID,
			CamGen:       "2",
			Snippets:     firstFresh.Snippets,
			RecentRuns:   recentRuns,
			Stale:        false,
			Inactive:     false,
			QualityRuns:  []QualityRun{},
			Calibrations: []CameraCalibration{},
		}
	}

	newest := candidates[0]
	return Vehicle{
		ID:           vid,
		Project:      project,
		Location:     location,
		Run:          newest.runFilename,
		CamGen:       "n/a",
		Snippets:     []Snippet{},
		RecentRuns:   recentRuns,
		Stale:        true,
		StaleReason:  lastReason,
		Inactive:     false,
		QualityRuns:  []QualityRun{},
		Calibrations: []CameraCalibration{},
	}
}

// ---------------------------------------------------------------------------
// Main refresh orchestration
// ---------------------------------------------------------------------------

// runRefresh fetches all fleet data and writes vehicles.json.
// It returns the resulting vehicles slice.
func runRefresh() ([]Vehicle, error) {
	ctx := context.Background()
	log.Println("refresh: fetching vehicles from Fleetio...")
	active, inactive, err := rfGetFleetioVehicles()
	if err != nil {
		return nil, fmt.Errorf("Fleetio: %w", err)
	}
	log.Printf("refresh: %d active, %d inactive vehicles", len(active), len(inactive))

	log.Println("refresh: obtaining URSA token...")
	token, err := rfGetUrsaToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("URSA token: %w", err)
	}

	log.Printf("refresh: querying ADP SQL for latest runs (last %d days)...", rfADPWindowDays)
	allForRuns := append(active, inactive...)
	allPrefixesForRuns := make([]string, len(allForRuns))
	for i, v := range allForRuns {
		allPrefixesForRuns[i] = v.filenamePrefix
	}
	runMap, err := rfGetLatestRuns(ctx, allPrefixesForRuns, token)
	if err != nil {
		return nil, fmt.Errorf("ADP latest runs: %w", err)
	}
	log.Printf("refresh: found runs for %d/%d vehicles", len(runMap), len(allForRuns))

	log.Println("refresh: building S3 client...")
	s3Client, err := rfNewS3Client(ctx)
	if err != nil {
		return nil, fmt.Errorf("S3 client: %w", err)
	}

	log.Println("refresh: checking S3 for .rrd snippets (parallel)...")
	type vehicleResult struct {
		idx     int
		vehicle Vehicle
	}
	totalVehicles := len(active) + len(inactive)
	results := make([]Vehicle, totalVehicles)
	ch := make(chan vehicleResult, totalVehicles)
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i, fv := range active {
		wg.Add(1)
		go func(idx int, fv rfFleetVehicle) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := rfRefreshVehicle(ctx, s3Client, fv, runMap[fv.filenamePrefix])
			ch <- vehicleResult{idx, v}
		}(i, fv)
	}
	// Inactive vehicles share the same pool — populate RecentRuns for the detail view,
	// then override triage-table fields so they still appear as inactive.
	for i, fv := range inactive {
		wg.Add(1)
		go func(idx int, fv rfFleetVehicle) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := rfRefreshVehicle(ctx, s3Client, fv, runMap[fv.filenamePrefix])
			v.Stale = true
			v.Inactive = true
			v.StaleReason = fmt.Sprintf("not active in fleet (%s)", fv.status)
			v.Snippets = []Snippet{}
			v.Run = ""
			v.CamGen = "n/a"
			ch <- vehicleResult{len(active) + idx, v}
		}(i, fv)
	}
	wg.Wait()
	close(ch)
	for r := range ch {
		results[r.idx] = r.vehicle
	}

	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })

	fresh := 0
	for _, r := range results {
		if !r.Stale {
			fresh++
		}
	}
	log.Printf("refresh: %d fresh, %d stale", fresh, len(results)-fresh)

	// Attach quality/frame-gen data for all vehicles (including inactive)
	allFleet := append(active, inactive...)
	allPrefixes := make([]string, len(allFleet))
	for i, v := range allFleet {
		allPrefixes[i] = v.filenamePrefix
	}
	prefixToVID := map[string]string{}
	vidToPrefix := map[string]string{}
	for _, v := range allFleet {
		prefixToVID[v.filenamePrefix] = v.vehicleID
		vidToPrefix[v.vehicleID] = v.filenamePrefix
	}

	log.Println("refresh: fetching quality runs from ORDC dashboard...")
	qualityRuns, err := rfGetQualityRuns(ctx, allPrefixes, token)
	if err != nil {
		log.Printf("refresh: warning: quality_runs failed: %v", err)
		qualityRuns = nil
	} else {
		total := 0
		for _, v := range qualityRuns {
			total += len(v)
		}
		log.Printf("refresh: %d run(s) with quality data across %d vehicle(s)", total, len(qualityRuns))
	}
	for i := range results {
		prefix := vidToPrefix[results[i].ID]
		if prefix == "" {
			prefix = results[i].ID
		}
		if qr, ok := qualityRuns[prefix]; ok {
			results[i].QualityRuns = qr
		}
	}

	log.Println("refresh: fetching frame gen issues (last 7 days)...")
	skippedRuns, err := rfGetSkippedRuns(ctx, allPrefixes, token)
	if err != nil {
		log.Printf("refresh: warning: skipped_runs failed: %v", err)
		skippedRuns = nil
	} else {
		log.Printf("refresh: %d skipped run(s) across %d vehicle(s)", func() int {
			n := 0
			for _, v := range skippedRuns {
				n += len(v)
			}
			return n
		}(), len(skippedRuns))
	}

	zeroSegRuns, err := rfGetZeroSegmentRuns(ctx, allPrefixes, token)
	if err != nil {
		log.Printf("refresh: warning: zero_segment_runs failed: %v", err)
		zeroSegRuns = nil
	} else {
		log.Printf("refresh: %d zero-segment run(s) across %d vehicle(s)", func() int {
			n := 0
			for _, v := range zeroSegRuns {
				n += len(v)
			}
			return n
		}(), len(zeroSegRuns))
	}

	convIssues, err := rfGetConversionIssues(ctx, allPrefixes, token)
	if err != nil {
		log.Printf("refresh: warning: conversion_issues failed: %v", err)
		convIssues = nil
	} else {
		log.Printf("refresh: %d conversion issue(s) across %d vehicle(s)", func() int {
			n := 0
			for _, v := range convIssues {
				n += len(v)
			}
			return n
		}(), len(convIssues))
	}

	log.Println("refresh: fetching calibration status...")
	calibStatus := rfGetCalibrationStatus(ctx, s3Client, allPrefixes)
	staleCalibCams := 0
	for _, cams := range calibStatus {
		for _, c := range cams {
			if c.Stale {
				staleCalibCams++
			}
		}
	}
	log.Printf("refresh: %d overdue camera(s) across %d vehicle(s)", staleCalibCams, len(calibStatus))

	for i := range results {
		prefix := vidToPrefix[results[i].ID]
		if prefix == "" {
			prefix = results[i].ID
		}
		if sr := skippedRuns[prefix]; sr != nil {
			results[i].SkippedRuns = sr
		}
		if zsr := zeroSegRuns[prefix]; zsr != nil {
			results[i].ZeroSegmentRuns = zsr
		}
		if ci := convIssues[prefix]; ci != nil {
			results[i].ConversionIssues = ci
		}
		if cal := calibStatus[prefix]; cal != nil {
			results[i].Calibrations = cal
		}
	}

	// Write vehicles.json
	outData, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal vehicles: %w", err)
	}
	if err := os.WriteFile("vehicles.json", outData, 0644); err != nil {
		return nil, fmt.Errorf("write vehicles.json: %w", err)
	}
	log.Printf("refresh: wrote vehicles.json (%d vehicles)", len(results))
	return results, nil
}

// ---------------------------------------------------------------------------
// Truck refresh — reads #bot-trucking-data-intelligence Slack channel
// ---------------------------------------------------------------------------

const (
	rfFrontierRerunBase  = "https://frontier.prod.applied.dev/data_explorer/v2/rerun?version=0.26&rrd="
	rfTruckSlackChannel  = "bot-trucking-data-intelligence"
	rfTruckMsgFetchLimit = 200
)

var rfFrontierCameras = []string{
	"AT360_FC_ROOF_FWD",
	"AT360_FL_ROOF_BWD",
	"AT360_FL_ROOF_FWD",
	"AT360_FR_ROOF_BWD",
	"AT360_FR_ROOF_FWD",
	"AT360_RC_BMPR_BWD",
	"QT128_FC_BMPR_FWD",
	"QT128_FL_ROOF_SIDE",
	"QT128_FR_ROOF_SIDE",
}

// Matches the S3 path in a frontier Rerun URL (handles angle-bracket Slack links too).
// Captures: (1) camera, (2) uuid, (3) filename.rrd
var rfTruckRRDRe = regexp.MustCompile(
	`rrd=s3://frontier-prod-data-intelligence-frame-gen/frontier_raw_data_triage/calibration/([^/]+)/([^/]+)/([^&\s<>|"]+\.rrd)`)

// Captures chunk number and start Unix timestamp from an RRD filename.
var rfTruckChunkRe = regexp.MustCompile(`_chunk_(\d+)_(\d+)s_to_`)

// runRefreshTrucks reads recent messages from the Slack channel
// #bot-trucking-data-intelligence, parses frontier Rerun URLs, and writes trucks.json.
func runRefreshTrucks() ([]Truck, error) {
	if slackBot == nil {
		return nil, fmt.Errorf("Slack bot not initialized — cannot read #%s", rfTruckSlackChannel)
	}
	client, err := slackBot.Client()
	if err != nil {
		return nil, fmt.Errorf("Slack client unavailable: %w", err)
	}

	// Find channel ID.
	channelID, err := rfFindSlackChannel(client, rfTruckSlackChannel)
	if err != nil {
		return nil, fmt.Errorf("find #%s: %w (bot may need channels:read scope)", rfTruckSlackChannel, err)
	}

	// Fetch recent messages.
	history, err := client.GetConversationHistory(&slackpkg.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     rfTruckMsgFetchLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("get history for #%s: %w (bot may need channels:history scope)", rfTruckSlackChannel, err)
	}
	log.Printf("trucks: fetched %d messages from #%s", len(history.Messages), rfTruckSlackChannel)

	result := rfParseTrucksFromMessages(history.Messages)
	log.Printf("trucks: parsed %d calibration run(s)", len(result))

	outData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal trucks: %w", err)
	}
	if err := os.WriteFile("trucks.json", outData, 0644); err != nil {
		return nil, fmt.Errorf("write trucks.json: %w", err)
	}
	return result, nil
}

// rfFindSlackChannel searches public channels for one matching name.
func rfFindSlackChannel(client *slackpkg.Client, name string) (string, error) {
	cursor := ""
	for {
		channels, nextCursor, err := client.GetConversations(&slackpkg.GetConversationsParameters{
			Types:  []string{"public_channel"},
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return "", err
		}
		for _, ch := range channels {
			if ch.Name == name {
				return ch.ID, nil
			}
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return "", fmt.Errorf("channel #%s not found", name)
}

type rfTruckChunkEntry struct {
	url string
	ts  int64
}

// rfParseTrucksFromMessages extracts truck calibration entries from Slack messages.
func rfParseTrucksFromMessages(messages []slackpkg.Message) []Truck {
	// byUUID[uuid][camera][chunkNum] = entry
	byUUID := map[string]map[string]map[int]rfTruckChunkEntry{}

	for _, msg := range messages {
		rfExtractTruckURLs(msg.Text, byUUID)
		// Also scan block text (Slack rich-text blocks may contain the links)
		for _, attachment := range msg.Attachments {
			rfExtractTruckURLs(attachment.Text, byUUID)
			rfExtractTruckURLs(attachment.Pretext, byUUID)
		}
	}

	staleCutoff := time.Now().UTC().Add(-rfStaleDays * 24 * time.Hour)

	var result []Truck
	for runUUID, cameraMap := range byUUID {
		// Pick earliest Unix timestamp across all chunks for the run date.
		var minTS int64
		for _, chunkMap := range cameraMap {
			for _, entry := range chunkMap {
				if entry.ts > 0 && (minTS == 0 || entry.ts < minTS) {
					minTS = entry.ts
				}
			}
		}
		var runTime time.Time
		if minTS > 0 {
			runTime = time.Unix(minTS, 0).UTC()
		} else {
			runTime = time.Now().UTC()
		}
		dateStr := runTime.Format("2006-01-02")

		var cameras []TruckCamera
		for _, cam := range rfFrontierCameras {
			chunkMap, ok := cameraMap[cam]
			if !ok {
				continue
			}
			var chunkNums []int
			for n := range chunkMap {
				chunkNums = append(chunkNums, n)
			}
			sort.Ints(chunkNums)

			var snippets []Snippet
			for _, n := range chunkNums {
				snippets = append(snippets, Snippet{
					Label: fmt.Sprintf("Chunk %d", n),
					URL:   chunkMap[n].url,
				})
			}
			group := "AT360"
			if strings.HasPrefix(cam, "QT128") {
				group = "QT128"
			}
			cameras = append(cameras, TruckCamera{Name: cam, Group: group, Snippets: snippets})
		}
		if len(cameras) == 0 {
			continue
		}
		result = append(result, Truck{
			UUID:    runUUID,
			Date:    dateStr,
			Stale:   runTime.Before(staleCutoff),
			Cameras: cameras,
		})
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Date > result[j].Date })
	return result
}

// rfExtractTruckURLs scans text for frontier Rerun URLs and populates byUUID.
func rfExtractTruckURLs(text string, byUUID map[string]map[string]map[int]rfTruckChunkEntry) {
	matches := rfTruckRRDRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		camera, uuid, filename := m[1], m[2], m[3]

		chunkNum := 1
		var ts int64
		if cm := rfTruckChunkRe.FindStringSubmatch(filename); cm != nil {
			chunkNum, _ = strconv.Atoi(cm[1])
			ts, _ = strconv.ParseInt(cm[2], 10, 64)
		}

		rerunURL := rfFrontierRerunBase + "s3://frontier-prod-data-intelligence-frame-gen/frontier_raw_data_triage/calibration/" + camera + "/" + uuid + "/" + filename

		if byUUID[uuid] == nil {
			byUUID[uuid] = map[string]map[int]rfTruckChunkEntry{}
		}
		if byUUID[uuid][camera] == nil {
			byUUID[uuid][camera] = map[int]rfTruckChunkEntry{}
		}
		byUUID[uuid][camera][chunkNum] = rfTruckChunkEntry{url: rerunURL, ts: ts}
	}
}
