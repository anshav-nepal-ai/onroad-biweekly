package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type triageCycle struct {
	ID             int     `json:"id"`
	CreatedAt      string  `json:"created_at"`
	StartedByName  string  `json:"started_by_name"`
	StartedByEmail string  `json:"started_by_email"`
	TriageDate     string  `json:"triage_date"`
	CompletedAt    *string `json:"completed_at"`
}

type triageAnnotationDB struct {
	CamQuality     string `json:"cam_quality"`
	CamNote        string `json:"cam_note"`
	DriveQuality   string `json:"drive_quality"`
	DriveNote      string `json:"drive_note"`
	TriageComments string `json:"triage_comments"`
	Included       bool   `json:"included"`
	LastEditedBy   string `json:"last_edited_by"`
	LastEditedAt   string `json:"last_edited_at"`
}

type actionItemDB struct {
	Content      string `json:"content"`
	LastEditedBy string `json:"last_edited_by"`
	LastEditedAt string `json:"last_edited_at"`
}

type activeEditorInfo struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	StartedAt  string `json:"started_at"`
	LastActive string `json:"last_active"`
}

type triageStateResp struct {
	Cycle        *triageCycle                  `json:"cycle"`
	Annotations  map[string]triageAnnotationDB `json:"annotations"`
	ActionItems  map[string]actionItemDB       `json:"action_items"`
	ActiveEditor *activeEditorInfo             `json:"active_editor"`
	LastUpdated  *string                       `json:"last_updated"`
	Vehicles     []Vehicle                     `json:"vehicles"` // nil = use live data
}

// ---------------------------------------------------------------------------
// DB helpers
// ---------------------------------------------------------------------------

func scanCycle(row interface{ Scan(...any) error }) (*triageCycle, error) {
	var c triageCycle
	var completedAt sql.NullString
	err := row.Scan(&c.ID, &c.CreatedAt, &c.StartedByName, &c.StartedByEmail, &c.TriageDate, &completedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if completedAt.Valid {
		c.CompletedAt = &completedAt.String
	}
	return &c, nil
}

func dbGetCurrentCycle() (*triageCycle, error) {
	return scanCycle(db.QueryRow(
		`SELECT id, created_at, started_by_name, started_by_email, triage_date::text,
		        to_char(completed_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM triage_cycles WHERE completed_at IS NULL ORDER BY id DESC LIMIT 1`,
	))
}

func dbGetCycle(id int) (*triageCycle, error) {
	return scanCycle(db.QueryRow(
		`SELECT id, created_at, started_by_name, started_by_email, triage_date::text,
		        to_char(completed_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM triage_cycles WHERE id = $1`, id,
	))
}

func dbGetAnnotations(cycleID int) map[string]triageAnnotationDB {
	out := map[string]triageAnnotationDB{}
	rows, err := db.Query(
		`SELECT vehicle_id, cam_quality, cam_note, drive_quality, drive_note,
		        triage_comments, included, last_edited_by, last_edited_at
		 FROM triage_annotations WHERE cycle_id = $1`, cycleID,
	)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var vid string
		var a triageAnnotationDB
		var lastAt time.Time
		if err := rows.Scan(&vid, &a.CamQuality, &a.CamNote, &a.DriveQuality, &a.DriveNote,
			&a.TriageComments, &a.Included, &a.LastEditedBy, &lastAt); err == nil {
			a.LastEditedAt = lastAt.Format(time.RFC3339)
			out[vid] = a
		}
	}
	return out
}

func dbGetActionItems(cycleID int) map[string]actionItemDB {
	out := map[string]actionItemDB{}
	rows, err := db.Query(
		`SELECT section, content, last_edited_by, last_edited_at FROM action_items WHERE cycle_id = $1`,
		cycleID,
	)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var section string
		var ai actionItemDB
		var lastAt time.Time
		if err := rows.Scan(&section, &ai.Content, &ai.LastEditedBy, &lastAt); err == nil {
			ai.LastEditedAt = lastAt.Format(time.RFC3339)
			out[section] = ai
		}
	}
	return out
}

func dbGetActiveEditor(cycleID int) *activeEditorInfo {
	var e activeEditorInfo
	var startedAt, lastActive time.Time
	err := db.QueryRow(
		`SELECT editor_name, editor_email, started_at, last_active_at
		 FROM edit_sessions
		 WHERE cycle_id = $1 AND is_active = TRUE
		   AND last_active_at > NOW() - INTERVAL '30 minutes'
		 ORDER BY id DESC LIMIT 1`, cycleID,
	).Scan(&e.Name, &e.Email, &startedAt, &lastActive)
	if err != nil {
		return nil
	}
	e.StartedAt = startedAt.Format(time.RFC3339)
	e.LastActive = lastActive.Format(time.RFC3339)
	return &e
}

func dbGetLastUpdated(cycleID int) *string {
	var ts time.Time
	err := db.QueryRow(
		`SELECT MAX(last_edited_at) FROM triage_annotations WHERE cycle_id = $1`, cycleID,
	).Scan(&ts)
	if err != nil || ts.IsZero() {
		return nil
	}
	s := ts.Format(time.RFC3339)
	return &s
}

func dbSnapshotVehicles(cycleID int, vs []Vehicle) {
	for _, v := range vs {
		data, err := json.Marshal(v)
		if err != nil {
			continue
		}
		db.Exec(
			`INSERT INTO triage_vehicles (cycle_id, vehicle_id, data)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (cycle_id, vehicle_id) DO NOTHING`,
			cycleID, v.ID, data,
		)
	}
}

func dbGetVehicles(cycleID int) []Vehicle {
	rows, err := db.Query(
		`SELECT data FROM triage_vehicles WHERE cycle_id = $1 ORDER BY vehicle_id`,
		cycleID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Vehicle
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var v Vehicle
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

func buildStateResp(cycleID int) triageStateResp {
	cycle, _ := dbGetCycle(cycleID)
	if cycle == nil {
		return triageStateResp{Annotations: map[string]triageAnnotationDB{}, ActionItems: map[string]actionItemDB{}}
	}
	return triageStateResp{
		Cycle:        cycle,
		Annotations:  dbGetAnnotations(cycleID),
		ActionItems:  dbGetActionItems(cycleID),
		ActiveEditor: dbGetActiveEditor(cycleID),
		LastUpdated:  dbGetLastUpdated(cycleID),
		Vehicles:     dbGetVehicles(cycleID),
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// GET /api/triage/current
func triageGetCurrentHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusOK, triageStateResp{
			Annotations: map[string]triageAnnotationDB{},
			ActionItems: map[string]actionItemDB{},
		})
		return
	}
	cycle, err := dbGetCurrentCycle()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if cycle == nil {
		c.JSON(http.StatusOK, triageStateResp{
			Annotations: map[string]triageAnnotationDB{},
			ActionItems: map[string]actionItemDB{},
		})
		return
	}
	c.JSON(http.StatusOK, buildStateResp(cycle.ID))
}

// GET /api/triage/cycles
func triageListCyclesHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusOK, []triageCycle{})
		return
	}
	rows, err := db.Query(
		`SELECT id, created_at, started_by_name, started_by_email, triage_date::text,
		        to_char(completed_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM triage_cycles ORDER BY id DESC`,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	var cycles []triageCycle
	for rows.Next() {
		var cy triageCycle
		var completedAt sql.NullString
		rows.Scan(&cy.ID, &cy.CreatedAt, &cy.StartedByName, &cy.StartedByEmail, &cy.TriageDate, &completedAt)
		if completedAt.Valid {
			cy.CompletedAt = &completedAt.String
		}
		cycles = append(cycles, cy)
	}
	if cycles == nil {
		cycles = []triageCycle{}
	}
	c.JSON(http.StatusOK, cycles)
}

// GET /api/triage/cycles/:id
func triageGetCycleHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence disabled"})
		return
	}
	var id int
	fmt.Sscan(c.Param("id"), &id)
	if id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	c.JSON(http.StatusOK, buildStateResp(id))
}

// POST /api/triage/start  body: {name, email}
func triageStartHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence disabled"})
		return
	}
	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" || req.Email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and email required"})
		return
	}

	// Deactivate any edit session on the current cycle
	if cur, _ := dbGetCurrentCycle(); cur != nil {
		db.Exec(`UPDATE edit_sessions SET is_active = FALSE WHERE cycle_id = $1`, cur.ID)
	}

	// Create new cycle
	var newID int
	err := db.QueryRow(
		`INSERT INTO triage_cycles (started_by_name, started_by_email, triage_date)
		 VALUES ($1, $2, CURRENT_DATE) RETURNING id`,
		req.Name, req.Email,
	).Scan(&newID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Seed action items from Jira
	go func() {
		tickets, err := vstabFetchTickets()
		if err != nil {
			log.Printf("triage start: VSTAB fetch failed: %v", err)
			return
		}
		var camLines, activeLines []string
		for _, t := range tickets {
			line := fmt.Sprintf("[%s] - %s | %s", t.VehicleID, t.Summary, t.URL)
			if t.VehicleID == "" {
				line = t.Summary + " | " + t.URL
			}
			if strings.Contains(strings.ToLower(t.Summary), "camera image quality") {
				camLines = append(camLines, line)
			} else {
				activeLines = append(activeLines, line)
			}
		}
		for section, lines := range map[string][]string{
			"camera_quality": camLines,
			"active_tickets": activeLines,
		} {
			if len(lines) == 0 {
				continue
			}
			db.Exec(
				`INSERT INTO action_items (cycle_id, section, content) VALUES ($1, $2, $3)
				 ON CONFLICT (cycle_id, section) DO UPDATE SET content = EXCLUDED.content`,
				newID, section, strings.Join(lines, "\n"),
			)
		}
	}()

	c.JSON(http.StatusOK, buildStateResp(newID))
}

// PATCH /api/triage/annotations  body: {cycle_id, vehicle_id, ...fields, editor_name, editor_email}
func triageUpsertAnnotationHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence disabled"})
		return
	}
	var req struct {
		CycleID        int    `json:"cycle_id"`
		VehicleID      string `json:"vehicle_id"`
		CamQuality     string `json:"cam_quality"`
		CamNote        string `json:"cam_note"`
		DriveQuality   string `json:"drive_quality"`
		DriveNote      string `json:"drive_note"`
		TriageComments string `json:"triage_comments"`
		Included       *bool  `json:"included"`
		EditorName     string `json:"editor_name"`
		EditorEmail    string `json:"editor_email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.CycleID == 0 || req.VehicleID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cycle_id and vehicle_id required"})
		return
	}
	included := true
	if req.Included != nil {
		included = *req.Included
	}
	editorBy := req.EditorName
	if req.EditorEmail != "" {
		editorBy = req.EditorName + " <" + req.EditorEmail + ">"
	}
	_, err := db.Exec(
		`INSERT INTO triage_annotations
		 (cycle_id, vehicle_id, cam_quality, cam_note, drive_quality, drive_note,
		  triage_comments, included, last_edited_by, last_edited_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NOW())
		 ON CONFLICT (cycle_id, vehicle_id) DO UPDATE SET
		   cam_quality=$3, cam_note=$4, drive_quality=$5, drive_note=$6,
		   triage_comments=$7, included=$8, last_edited_by=$9, last_edited_at=NOW()`,
		req.CycleID, req.VehicleID, req.CamQuality, req.CamNote,
		req.DriveQuality, req.DriveNote, req.TriageComments, included, editorBy,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// PATCH /api/triage/action-items  body: {cycle_id, section, content, editor_name, editor_email}
func triageUpsertActionItemHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence disabled"})
		return
	}
	var req struct {
		CycleID     int    `json:"cycle_id"`
		Section     string `json:"section"`
		Content     string `json:"content"`
		EditorName  string `json:"editor_name"`
		EditorEmail string `json:"editor_email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.CycleID == 0 || req.Section == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cycle_id and section required"})
		return
	}
	editorBy := req.EditorName
	if req.EditorEmail != "" {
		editorBy = req.EditorName + " <" + req.EditorEmail + ">"
	}
	_, err := db.Exec(
		`INSERT INTO action_items (cycle_id, section, content, last_edited_by, last_edited_at)
		 VALUES ($1,$2,$3,$4,NOW())
		 ON CONFLICT (cycle_id, section) DO UPDATE SET
		   content=$3, last_edited_by=$4, last_edited_at=NOW()`,
		req.CycleID, req.Section, req.Content, editorBy,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// POST /api/triage/edit-session/start  body: {cycle_id, name, email}
func triageEditSessionStartHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence disabled"})
		return
	}
	var req struct {
		CycleID int    `json:"cycle_id"`
		Name    string `json:"name"`
		Email   string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.CycleID == 0 || req.Name == "" || req.Email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cycle_id, name, and email required"})
		return
	}
	existing := dbGetActiveEditor(req.CycleID)
	if existing != nil && existing.Email != req.Email {
		c.JSON(http.StatusConflict, gin.H{"editor": existing})
		return
	}
	// Deactivate old sessions, start fresh
	db.Exec(`UPDATE edit_sessions SET is_active = FALSE WHERE cycle_id = $1`, req.CycleID)
	_, err := db.Exec(
		`INSERT INTO edit_sessions (cycle_id, editor_name, editor_email) VALUES ($1,$2,$3)`,
		req.CycleID, req.Name, req.Email,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// POST /api/triage/edit-session/heartbeat  body: {cycle_id, email}
func triageEditSessionHeartbeatHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	var req struct {
		CycleID int    `json:"cycle_id"`
		Email   string `json:"email"`
	}
	c.ShouldBindJSON(&req)
	if req.CycleID != 0 && req.Email != "" {
		db.Exec(
			`UPDATE edit_sessions SET last_active_at = NOW()
			 WHERE cycle_id = $1 AND editor_email = $2 AND is_active = TRUE`,
			req.CycleID, req.Email,
		)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// POST /api/triage/edit-session/done  body: {cycle_id, email}
func triageEditSessionDoneHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	var req struct {
		CycleID int    `json:"cycle_id"`
		Email   string `json:"email"`
	}
	c.ShouldBindJSON(&req)
	if req.CycleID != 0 && req.Email != "" {
		db.Exec(
			`UPDATE edit_sessions SET is_active = FALSE
			 WHERE cycle_id = $1 AND editor_email = $2`,
			req.CycleID, req.Email,
		)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// POST /api/triage/complete  body: {cycle_id}
// Saves a vehicle snapshot and marks the cycle as completed.
func triageCompleteHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence disabled"})
		return
	}
	var req struct {
		CycleID int `json:"cycle_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.CycleID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cycle_id required"})
		return
	}

	vehiclesMu.RLock()
	snapshot := make([]Vehicle, len(vehicles))
	copy(snapshot, vehicles)
	vehiclesMu.RUnlock()

	// Clear any existing snapshot for this cycle then re-insert
	db.Exec(`DELETE FROM triage_vehicles WHERE cycle_id = $1`, req.CycleID)
	dbSnapshotVehicles(req.CycleID, snapshot)

	if _, err := db.Exec(
		`UPDATE triage_cycles SET completed_at = NOW() WHERE id = $1`, req.CycleID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, buildStateResp(req.CycleID))
}

// POST /api/triage/refresh-tickets  body: {cycle_id}
// Re-fetches active VSTAB tickets from Jira and updates the action_items rows.
func triageRefreshTicketsHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence disabled"})
		return
	}
	var req struct {
		CycleID int `json:"cycle_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.CycleID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cycle_id required"})
		return
	}
	tickets, err := vstabFetchTickets()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var camLines, activeLines []string
	for _, t := range tickets {
		line := fmt.Sprintf("[%s] - %s | %s", t.VehicleID, t.Summary, t.URL)
		if t.VehicleID == "" {
			line = t.Summary + " | " + t.URL
		}
		if strings.Contains(strings.ToLower(t.Summary), "camera image quality") {
			camLines = append(camLines, line)
		} else {
			activeLines = append(activeLines, line)
		}
	}
	for section, lines := range map[string][]string{
		"camera_quality": camLines,
		"active_tickets": activeLines,
	} {
		if len(lines) == 0 {
			continue
		}
		db.Exec(
			`INSERT INTO action_items (cycle_id, section, content) VALUES ($1, $2, $3)
			 ON CONFLICT (cycle_id, section) DO UPDATE SET content = EXCLUDED.content`,
			req.CycleID, section, strings.Join(lines, "\n"),
		)
	}

	// Bust the camera-tickets in-memory cache so the next GET /api/triage/camera-tickets
	// call fetches fresh data from Jira instead of serving the stale cached copy.
	camTicketsCacheMu.Lock()
	camTicketsCacheAt = time.Time{}
	camTicketsCacheMu.Unlock()

	c.JSON(http.StatusOK, buildStateResp(req.CycleID))
}

// POST /api/triage/post-slack  body: {cycle_id}
// Returns the formatted Slack message without posting it — posting is done via the /post-triage-slack skill.
func triagePostSlackHandler(c *gin.Context) {
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence disabled"})
		return
	}
	var req struct {
		CycleID int `json:"cycle_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.CycleID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cycle_id required"})
		return
	}

	state := buildStateResp(req.CycleID)
	if state.Cycle == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "cycle not found"})
		return
	}

	// Unhealthy = non-empty lines in "new" and "ongoing" action items.
	unhealthy := 0
	for _, key := range []string{"new", "ongoing"} {
		for _, line := range strings.Split(state.ActionItems[key].Content, "\n") {
			if strings.TrimSpace(line) != "" {
				unhealthy++
			}
		}
	}

	// Use snapshot vehicles for completed cycles; fall back to live.
	countVehicles := state.Vehicles
	if len(countVehicles) == 0 {
		vehiclesMu.RLock()
		countVehicles = make([]Vehicle, len(vehicles))
		copy(countVehicles, vehicles)
		vehiclesMu.RUnlock()
	}

	oos := 0
	for _, v := range countVehicles {
		if v.Inactive {
			oos++
		}
	}
	healthy := len(countVehicles) - oos - unhealthy
	if healthy < 0 {
		healthy = 0
	}

	// Format date: "2026-08-11" -> "08-11-26"
	slackDate := state.Cycle.TriageDate
	if t, err := time.Parse("2006-01-02", state.Cycle.TriageDate); err == nil {
		slackDate = t.Format("01-02-06")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(":thread: *Bi-Weekly Onroad Cars Data Quality Triage* :spiral_calendar_pad: *%s*\n", slackDate))
	sb.WriteString("*Vehicle Summary:*\n")
	sb.WriteString(fmt.Sprintf("  • Vehicles Healthy : %d\n", healthy))
	sb.WriteString(fmt.Sprintf("  • Vehicles Unhealthy : %d\n", unhealthy))
	sb.WriteString(fmt.Sprintf("  • Out of Service/Inactive/Pending Return: %d\n", oos))

	sections := []struct {
		key    string
		header string
	}{
		{"new", ":new-new: *New*"},
		{"ongoing", ":arrows_counterclockwise: *Ongoing*"},
		{"camera_quality", ":camera: *Camera Image Quality Tickets*"},
		{"active_tickets", ":ticket: *Active Tickets*"},
	}

	hasAny := false
	for _, s := range sections {
		if strings.TrimSpace(state.ActionItems[s.key].Content) != "" {
			hasAny = true
			break
		}
	}
	if hasAny {
		sb.WriteString("*Action Items:*\n")
		for _, s := range sections {
			content := strings.TrimSpace(state.ActionItems[s.key].Content)
			if content == "" {
				continue
			}
			sb.WriteString(s.header + "\n")
			for _, line := range strings.Split(content, "\n") {
				if t := strings.TrimSpace(line); t != "" {
					sb.WriteString("  • " + slackFormatItem(t) + "\n")
				}
			}
		}
	}

	sb.WriteString("<https://onroad-biweekly.experimental.apps.applied.dev|Onroad Biweekly Dashboard>\n")
	sb.WriteString("> :information_source: *Note: The \"Vehicles Unhealthy\" count is based on the most recent run observed in <#C085AAWTQD7|bot-data-intelligence>. Vehicles listed under Action Items may have already been remediated or are actively in progress — such changes will be reflected in the next triage cycle.*\n")
	sb.WriteString("> :information_source: *All calibration information should be directed to either #bot-di-auto-calibration (to search for past redeployments, use `autocal redeploy in:bot-di-auto-calibration`) or #bot-di-recalibration-requests.*\n")

	c.JSON(http.StatusOK, gin.H{"message": sb.String()})
}

var (
	camTicketsCache    string
	camTicketsCacheAt  time.Time
	camTicketsCacheMu  sync.Mutex
)

// GET /api/triage/camera-tickets — fetches camera quality tickets live from Jira (5-min cache).
// Used by the fleet view to show CQ flags without needing an active triage cycle.
// Pass ?force=true to bypass the cache and fetch fresh data immediately.
func triageCameraTicketsHandler(c *gin.Context) {
	force := c.Query("force") == "true"

	camTicketsCacheMu.Lock()
	if !force && time.Since(camTicketsCacheAt) < 5*time.Minute && camTicketsCache != "" {
		content := camTicketsCache
		camTicketsCacheMu.Unlock()
		c.JSON(http.StatusOK, gin.H{"content": content})
		return
	}
	camTicketsCacheMu.Unlock()

	tickets, err := vstabFetchTickets()
	if err != nil {
		log.Printf("camera-tickets: jira fetch failed: %v", err)
		c.JSON(http.StatusOK, gin.H{"content": ""})
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

	c.JSON(http.StatusOK, gin.H{"content": content})
}

// slackFormatItem converts "text | url" to "text, <url|Jira>", leaving plain text as-is.
func slackFormatItem(line string) string {
	parts := strings.SplitN(line, " | ", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]) + ", <" + strings.TrimSpace(parts[1]) + "|Jira>"
	}
	return line
}
