package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var cachedHTML string
var cachedSetupSh string

const (
	dbPath     = "students.json"
	configPath = "config.json"
	port       = ":2225"
)

type StaticVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type AppConfig struct {
	StaticVars []StaticVar `json:"static_vars"`
	CSVText    string      `json:"csv_text"`
	CSVHeaders []string    `json:"csv_headers"`
	CSVRows    [][]string  `json:"csv_rows"`
	IsOpen     bool        `json:"is_open"`
}

type Student struct {
	Name          string `json:"name"`
	UUIDDomain    string `json:"uuid_domain"`
	FirstAccessed string `json:"first_accessed"`
	LastAccessed  string `json:"last_accessed"`
	Payload       string `json:"payload"`
	CSVRowIndex   *int   `json:"csv_row_index,omitempty"`
}

type DB struct {
	Students []Student `json:"students"`
}

type rosterRow struct {
	Name          string `json:"name"`
	UUIDDomain    string `json:"uuid_domain"`
	FirstAccessed string `json:"first_accessed"`
	LastAccessed  string `json:"last_accessed"`
}

type sseState struct {
	IsOpen     bool        `json:"is_open"`
	SlotsTotal int         `json:"slots_total"`
	SlotsUsed  int         `json:"slots_used"`
	Students   []rosterRow `json:"students"`
}

// --- SSE broker ---

type sseBroker struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

var broker = &sseBroker{clients: make(map[chan []byte]struct{})}

func (b *sseBroker) subscribe() chan []byte {
	ch := make(chan []byte, 4)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *sseBroker) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *sseBroker) broadcast(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- data:
		default:
		}
	}
}

// --- Persistence ---

var mu sync.Mutex

func loadDB() (DB, error) {
	var db DB
	data, err := os.ReadFile(dbPath)
	if os.IsNotExist(err) {
		return DB{Students: []Student{}}, nil
	}
	if err != nil {
		return db, err
	}
	return db, json.Unmarshal(data, &db)
}

func saveDB(db DB) error {
	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dbPath, data, 0644)
}

func loadConfig() (AppConfig, error) {
	var cfg AppConfig
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return AppConfig{}, nil
	}
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(data, &cfg)
}

func saveConfig(cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

// --- State helpers (callers must hold mu) ---

func currentState() ([]byte, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	db, err := loadDB()
	if err != nil {
		return nil, err
	}
	slotsUsed := 0
	rows := make([]rosterRow, 0, len(db.Students))
	for _, s := range db.Students {
		if s.CSVRowIndex != nil {
			slotsUsed++
		}
		rows = append(rows, rosterRow{s.Name, s.UUIDDomain, s.FirstAccessed, s.LastAccessed})
	}
	return json.Marshal(sseState{
		IsOpen:     cfg.IsOpen,
		SlotsTotal: len(cfg.CSVRows),
		SlotsUsed:  slotsUsed,
		Students:   rows,
	})
}

func broadcastState() {
	data, err := currentState()
	if err != nil {
		log.Printf("ERROR building state: %v", err)
		return
	}
	broker.broadcast(data)
}

func buildPayload(cfg AppConfig, rowIdx int, name, uuidDomain string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# PCDS config for %s (%s)\n", name, uuidDomain)
	for _, sv := range cfg.StaticVars {
		fmt.Fprintf(&sb, "export %s=%q\n", sv.Key, sv.Value)
	}
	if rowIdx >= 0 && rowIdx < len(cfg.CSVRows) {
		row := cfg.CSVRows[rowIdx]
		for i, h := range cfg.CSVHeaders {
			if i < len(row) {
				fmt.Fprintf(&sb, "export %s=%q\n", h, row[i])
			}
		}
	}
	return sb.String()
}

// --- Handlers ---

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, cachedHTML)
}

func handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		StaticVars []StaticVar `json:"static_vars"`
		CSVText    string      `json:"csv_text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	var headers []string
	var rows [][]string
	if trimmed := strings.TrimSpace(req.CSVText); trimmed != "" {
		reader := csv.NewReader(strings.NewReader(trimmed))
		reader.TrimLeadingSpace = true
		records, err := reader.ReadAll()
		if err != nil {
			http.Error(w, "invalid CSV: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(records) > 0 {
			headers = records[0]
		}
		if len(records) > 1 {
			rows = records[1:]
		}
	}

	if req.StaticVars == nil {
		req.StaticVars = []StaticVar{}
	}

	mu.Lock()
	defer mu.Unlock()

	cfg := AppConfig{
		StaticVars: req.StaticVars,
		CSVText:    req.CSVText,
		CSVHeaders: headers,
		CSVRows:    rows,
		IsOpen:     true,
	}
	if err := saveConfig(cfg); err != nil {
		http.Error(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	broadcastState()
	w.WriteHeader(http.StatusOK)
}

func handleConfigData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	cfg, err := loadConfig()
	mu.Unlock()
	if err != nil {
		http.Error(w, "error reading config", http.StatusInternalServerError)
		return
	}
	if cfg.StaticVars == nil {
		cfg.StaticVars = []StaticVar{}
	}
	resp := struct {
		StaticVars  []StaticVar `json:"static_vars"`
		CSVText     string      `json:"csv_text"`
		CSVRowCount int         `json:"csv_row_count"`
		IsOpen      bool        `json:"is_open"`
	}{
		StaticVars:  cfg.StaticVars,
		CSVText:     cfg.CSVText,
		CSVRowCount: len(cfg.CSVRows),
		IsOpen:      cfg.IsOpen,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name       string `json:"name"`
		UUIDDomain string `json:"uuid_domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.UUIDDomain) == "" {
		http.Error(w, "name and uuid_domain are required", http.StatusBadRequest)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	cfg, err := loadConfig()
	if err != nil {
		log.Printf("ERROR loading config: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if !cfg.IsOpen {
		http.Error(w, "server not ready — ask your instructor to start the session", http.StatusServiceUnavailable)
		return
	}

	db, err := loadDB()
	if err != nil {
		log.Printf("ERROR loading %s: %v", dbPath, err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for i, s := range db.Students {
		if s.UUIDDomain == req.UUIDDomain {
			db.Students[i].Name = req.Name
			db.Students[i].LastAccessed = now
			if err := saveDB(db); err != nil {
				log.Printf("ERROR saving %s: %v", dbPath, err)
			}
			log.Printf("CACHE HIT   %s (%s)", req.Name, req.UUIDDomain)
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, s.Payload)
			broadcastState()
			return
		}
	}

	used := make(map[int]bool)
	for _, s := range db.Students {
		if s.CSVRowIndex != nil {
			used[*s.CSVRowIndex] = true
		}
	}
	rowIdx := -1
	for i := range cfg.CSVRows {
		if !used[i] {
			rowIdx = i
			break
		}
	}
	if rowIdx == -1 {
		log.Printf("POOL EMPTY  %s (%s)", req.Name, req.UUIDDomain)
		http.Error(w, "all slots are taken — the configuration pool is exhausted; contact your instructor", http.StatusInternalServerError)
		return
	}

	payload := buildPayload(cfg, rowIdx, req.Name, req.UUIDDomain)
	idx := rowIdx
	student := Student{
		Name:          req.Name,
		UUIDDomain:    req.UUIDDomain,
		FirstAccessed: now,
		LastAccessed:  now,
		Payload:       payload,
		CSVRowIndex:   &idx,
	}
	db.Students = append(db.Students, student)
	if err := saveDB(db); err != nil {
		log.Printf("ERROR saving %s: %v", dbPath, err)
	}
	log.Printf("CACHE MISS  %s (%s) → slot %d", req.Name, req.UUIDDomain, rowIdx)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, payload)
	broadcastState()
}

func handleRoster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	cfg, _ := loadConfig()
	db, err := loadDB()
	mu.Unlock()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	if !cfg.IsOpen {
		fmt.Fprintln(w, "PCDS: server not yet configured.")
		return
	}
	slotsUsed := 0
	for _, s := range db.Students {
		if s.CSVRowIndex != nil {
			slotsUsed++
		}
	}
	fmt.Fprintf(w, "Slots: %d used / %d total\n\n", slotsUsed, len(cfg.CSVRows))
	if len(db.Students) == 0 {
		fmt.Fprintln(w, "No students registered yet.")
		return
	}
	fmt.Fprintf(w, "%-4s  %-25s  %-50s  %s\n", "SLOT", "NAME", "UUID DOMAIN", "REGISTERED")
	fmt.Fprintln(w, strings.Repeat("-", 95))
	for _, s := range db.Students {
		slot := "—"
		if s.CSVRowIndex != nil {
			slot = fmt.Sprintf("%d", *s.CSVRowIndex+1)
		}
		fmt.Fprintf(w, "%-4s  %-25s  %-50s  %s\n", slot, s.Name, s.UUIDDomain, s.FirstAccessed)
	}
}

func handleSetupScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cachedSetupSh == "" {
		http.Error(w, "setup script not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	fmt.Fprint(w, cachedSetupSh)
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	os.Remove(configPath)
	os.Remove(dbPath)
	broadcastState()
	w.WriteHeader(http.StatusOK)
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	mu.Lock()
	data, err := currentState()
	mu.Unlock()
	if err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	ch := broker.subscribe()
	defer broker.unsubscribe(ch)
	for {
		select {
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func main() {
	fqdn := "YOUR_INSTRUCTOR_FQDN"
	if out, err := exec.Command("hostname", "--fqdn").Output(); err == nil {
		fqdn = strings.TrimSpace(string(out))
	}
	cachedHTML = strings.ReplaceAll(dashboardHTML, "{{FQDN}}", fqdn)

	if src, err := os.ReadFile("client_setup.sh"); err == nil {
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "INSTRUCTOR_FQDN=") {
				lines[i] = `INSTRUCTOR_FQDN="` + fqdn + `"`
				break
			}
		}
		cachedSetupSh = strings.Join(lines, "\n")
		log.Printf("setup.sh ready (FQDN: %s)", fqdn)
	} else {
		log.Printf("WARN: could not read client_setup.sh: %v", err)
	}

	http.HandleFunc("/", handleDashboard)
	http.HandleFunc("/config", handleConfigSave)
	http.HandleFunc("/config-data", handleConfigData)
	http.HandleFunc("/get-config", handleGetConfig)
	http.HandleFunc("/roster", handleRoster)
	http.HandleFunc("/setup.sh", handleSetupScript)
	http.HandleFunc("/reset", handleReset)
	http.HandleFunc("/events", handleEvents)
	log.Printf("PCDS listening on %s", port)
	log.Fatal(http.ListenAndServe(port, nil))
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PCDS</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: 'Segoe UI', system-ui, sans-serif;
  background: #0f1117;
  color: #e2e8f0;
  min-height: 100vh;
  padding: 2rem;
  max-width: 960px;
  margin: 0 auto;
}
h1 { font-size: 1.4rem; font-weight: 600; letter-spacing: 0.04em; }
h1 span { color: #63b3ed; }
h2 {
  font-size: 0.72rem;
  font-weight: 600;
  color: #a0aec0;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  margin-bottom: 0.75rem;
}
button {
  cursor: pointer;
  border: none;
  border-radius: 4px;
  font-size: 0.85rem;
  padding: 0.45rem 0.9rem;
  transition: background 0.15s;
  font-family: inherit;
}
.btn-primary {
  background: #3182ce;
  color: #fff;
  font-weight: 600;
  padding: 0.6rem 1.4rem;
  font-size: 0.9rem;
}
.btn-primary:hover:not(:disabled) { background: #2b6cb0; }
.btn-primary:disabled { background: #4a5568; cursor: not-allowed; }
.btn-secondary { background: #2d3748; color: #a0aec0; }
.btn-secondary:hover { background: #374151; color: #e2e8f0; }
.btn-ghost { background: transparent; color: #718096; padding: 0.25rem 0.5rem; font-size: 1rem; line-height: 1; }
.btn-ghost:hover { color: #fc8181; }
input[type=text] {
  background: #1a202c;
  border: 1px solid #2d3748;
  border-radius: 4px;
  color: #e2e8f0;
  font-size: 0.85rem;
  padding: 0.45rem 0.7rem;
  outline: none;
  width: 100%;
  font-family: inherit;
  transition: border-color 0.15s;
}
input[type=text]:focus { border-color: #4a90d9; }
input[type=text]::placeholder { color: #4a5568; }

/* loading */
#v-loading { color: #4a5568; font-style: italic; font-size: 0.85rem; }

/* config view */
#v-config header { display: flex; align-items: center; justify-content: space-between; margin-bottom: 2rem; }
.card {
  background: #1a202c;
  border: 1px solid #2d3748;
  border-radius: 8px;
  padding: 1.25rem 1.5rem;
  margin-bottom: 1.25rem;
}
.card-header { display: flex; align-items: center; justify-content: space-between; margin-bottom: 0.75rem; }
.hint { font-size: 0.82rem; color: #718096; line-height: 1.5; margin-bottom: 0.75rem; }
.static-row {
  display: grid;
  grid-template-columns: 1fr 1.4fr auto;
  gap: 0.5rem;
  margin-bottom: 0.5rem;
  align-items: center;
}
.static-row .key-in { font-family: 'Courier New', monospace; font-size: 0.82rem; }
textarea#csv-input {
  width: 100%;
  background: #0f1117;
  border: 1px solid #2d3748;
  border-radius: 4px;
  color: #9ae6b4;
  font-family: 'Courier New', monospace;
  font-size: 0.82rem;
  line-height: 1.6;
  padding: 0.75rem;
  resize: vertical;
  min-height: 150px;
  outline: none;
  transition: border-color 0.15s;
}
textarea#csv-input:focus { border-color: #4a90d9; }
textarea#csv-input::placeholder { color: #1e3a1e; }
#csv-preview { margin-top: 0.5rem; font-size: 0.82rem; min-height: 1.2em; }
#csv-preview.ok   { color: #68d391; }
#csv-preview.warn { color: #f6ad55; }
.config-actions { margin-top: 1.5rem; display: flex; gap: 0.75rem; align-items: center; }
#save-error { color: #fc8181; font-size: 0.85rem; }

/* setup command card */
.cmd-box {
  display: flex; align-items: center; gap: 0.75rem; margin-top: 0.5rem;
  background: #0f1117; border: 1px solid #2d3748; border-radius: 4px;
  padding: 0.6rem 0.75rem;
}
.cmd-box code {
  flex: 1; font-family: 'Courier New', monospace; font-size: 0.88rem;
  color: #9ae6b4; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
#btn-copy { background: #2d3748; color: #a0aec0; min-width: 64px; }
#btn-copy:hover { background: #374151; color: #e2e8f0; }
.btn-danger { background: #2d1a1a; color: #fc8181; }
.btn-danger:hover { background: #3d2020; color: #feb2b2; }

/* roster view */
#v-roster header { display: flex; align-items: center; justify-content: space-between; margin-bottom: 0.75rem; }
.header-right { display: flex; align-items: center; gap: 1rem; }
#slots-badge {
  font-size: 0.82rem;
  background: #1a202c;
  border: 1px solid #2d3748;
  border-radius: 20px;
  padding: 0.28rem 0.8rem;
  color: #a0aec0;
}
#slots-badge strong { color: #63b3ed; }
#status-indicator { display: flex; align-items: center; gap: 0.4rem; font-size: 0.82rem; color: #718096; }
#dot { width: 8px; height: 8px; border-radius: 50%; background: #68d391; }
#dot.live { animation: pulse 2s infinite; }
@keyframes pulse { 0%,100%{opacity:1} 50%{opacity:0.3} }
.roster-toolbar { display: flex; align-items: center; justify-content: space-between; margin-bottom: 1rem; }
#roster-count { font-size: 0.88rem; color: #a0aec0; min-height: 1.4em; }
#roster-count strong { color: #63b3ed; }
table { width: 100%; border-collapse: collapse; font-size: 0.88rem; }
thead th {
  text-align: left; padding: 0.55rem 1rem;
  background: #1a202c; color: #718096;
  font-weight: 500; letter-spacing: 0.07em;
  text-transform: uppercase; font-size: 0.72rem;
  border-bottom: 1px solid #2d3748;
}
thead th.col-num { text-align: right; }
tbody tr { border-bottom: 1px solid #1a202c; transition: background 0.15s; }
tbody tr:hover { background: #1a202c; }
tbody tr.flash { animation: flash 1.8s ease-out; }
@keyframes flash { 0%{background:#1c3a2a} 100%{background:transparent} }
td { padding: 0.7rem 1rem; }
td.num { color: #4a5568; text-align: right; width: 2.5rem; }
td.name { font-weight: 500; }
td.uuid { font-family: 'Courier New', monospace; color: #718096; font-size: 0.8rem; }
td.time { color: #718096; font-size: 0.8rem; }
#empty-roster { text-align: center; padding: 3rem 1rem; color: #4a5568; font-style: italic; }
</style>
</head>
<body>

<div id="v-loading">Connecting…</div>

<div id="v-config" hidden>
  <header>
    <h1>PCDS <span>Setup</span></h1>
  </header>

  <div class="card">
    <div class="card-header">
      <h2>Static Variables</h2>
      <button class="btn-secondary" id="btn-add-static">+ Add Variable</button>
    </div>
    <p class="hint">Same value on every student machine — tenant ID, subscription ID, region, etc.</p>
    <div id="static-rows"></div>
  </div>

  <div class="card">
    <h2>Unique Variables</h2>
    <p class="hint">
      First row = variable names. Each data row = one student slot assigned first-come first-served.
      Values in the same row are always assigned together.
    </p>
    <textarea id="csv-input"
      placeholder="ARM_CLIENT_ID,ARM_CLIENT_SECRET,ARM_RESOURCE_GROUP&#10;aaaaa-111,s3cr3t1,rg-student-01&#10;bbbbb-222,s3cr3t2,rg-student-02"></textarea>
    <div id="csv-preview"></div>
  </div>

  <div class="config-actions">
    <button class="btn-primary" id="btn-save">Save &amp; Open for Students</button>
    <span id="save-error"></span>
  </div>
</div>

<div id="v-roster" hidden>
  <header>
    <h1>PCDS <span>Roster</span></h1>
    <div class="header-right">
      <div id="slots-badge"></div>
      <div id="status-indicator">
        <div id="dot"></div>
        <span id="status-text">Connecting…</span>
      </div>
    </div>
  </header>

  <div class="card" style="margin-bottom:1.25rem">
    <h2>Student Setup Command</h2>
    <p class="hint">Ask students to run this in their terminal. They&#39;ll be prompted for their name.</p>
    <div class="cmd-box">
      <code id="setup-cmd">source &lt;(curl -s http://{{FQDN}}:2225/setup.sh)</code>
      <button id="btn-copy" onclick="copyCmd()">Copy</button>
    </div>
  </div>

  <div class="roster-toolbar">
    <div id="roster-count"></div>
    <div style="display:flex;gap:0.5rem">
      <button class="btn-secondary" id="btn-edit">&#9881; Edit Config</button>
      <button class="btn-danger" id="btn-reset">&#10006; Reset</button>
    </div>
  </div>
  <table>
    <thead>
      <tr>
        <th class="col-num">#</th>
        <th>Name</th>
        <th>UUID Domain</th>
        <th>Registered</th>
        <th>Last Seen</th>
      </tr>
    </thead>
    <tbody id="tbody"></tbody>
  </table>
  <div id="empty-roster">Waiting for students…</div>
</div>

<script>
function esc(s) {
  return String(s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}
function fmt(iso) {
  if (!iso) return '—';
  return new Date(iso).toLocaleString(undefined,{month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit'});
}

// --- view ---
let phase = 'loading';
function show(id) {
  ['v-loading','v-config','v-roster'].forEach(v => {
    document.getElementById(v).hidden = (v !== id);
  });
  phase = id.replace('v-','');
}

// --- SSE ---
let appState = null;
let knownUUIDs = new Set();

function connect() {
  const es = new EventSource('/events');
  es.onopen = () => setDot('live');
  es.onmessage = e => onState(JSON.parse(e.data));
  es.onerror = () => { setDot('error'); es.close(); setTimeout(connect, 3000); };
}

function onState(state) {
  appState = state;
  if (phase === 'loading') {
    if (state.is_open) {
      show('v-roster');
    } else {
      show('v-config');
      if (document.getElementById('static-rows').children.length === 0) {
        addStaticRow('', '');
      }
    }
  } else if (!state.is_open && phase === 'roster') {
    document.getElementById('static-rows').innerHTML = '';
    addStaticRow('', '');
    document.getElementById('csv-input').value = '';
    updateCSVPreview();
    show('v-config');
  }
  updateRosterUI(state);
}

function setDot(s) {
  const dot = document.getElementById('dot');
  const txt = document.getElementById('status-text');
  if (s === 'live') {
    dot.className = 'live'; dot.style.background = '#68d391'; txt.textContent = 'Live';
  } else {
    dot.className = ''; dot.style.background = '#fc8181'; txt.textContent = 'Reconnecting…';
  }
}

// --- roster UI ---
function updateRosterUI(state) {
  const remaining = state.slots_total - state.slots_used;
  const badge = document.getElementById('slots-badge');
  if (state.slots_total > 0) {
    badge.innerHTML = '<strong>' + remaining + '</strong> of ' + state.slots_total + ' slots remaining';
  } else {
    badge.textContent = 'No slots configured';
  }

  const students = state.students || [];
  const countEl = document.getElementById('roster-count');
  countEl.innerHTML = students.length
    ? '<strong>' + students.length + '</strong> student' + (students.length === 1 ? '' : 's') + ' registered'
    : '';

  const tbody = document.getElementById('tbody');
  const empty = document.getElementById('empty-roster');
  if (!students.length) { tbody.innerHTML = ''; empty.hidden = false; return; }
  empty.hidden = true;
  const frag = document.createDocumentFragment();
  students.forEach((s, i) => {
    const isNew = !knownUUIDs.has(s.uuid_domain);
    knownUUIDs.add(s.uuid_domain);
    const tr = document.createElement('tr');
    if (isNew) tr.className = 'flash';
    tr.innerHTML =
      '<td class="num">' + (i+1) + '</td>' +
      '<td class="name">' + esc(s.name) + '</td>' +
      '<td class="uuid">' + esc(s.uuid_domain) + '</td>' +
      '<td class="time">' + fmt(s.first_accessed) + '</td>' +
      '<td class="time">' + fmt(s.last_accessed) + '</td>';
    frag.appendChild(tr);
  });
  tbody.innerHTML = '';
  tbody.appendChild(frag);
}

// --- config form ---
function addStaticRow(key, val) {
  const row = document.createElement('div');
  row.className = 'static-row';
  row.innerHTML =
    '<input type="text" class="key-in" placeholder="VARIABLE_NAME" value="' + esc(key) + '">' +
    '<input type="text" class="val-in" placeholder="value" value="' + esc(val) + '">' +
    '<button class="btn-ghost" title="Remove" onclick="this.parentElement.remove()">×</button>';
  document.getElementById('static-rows').appendChild(row);
}

document.getElementById('btn-add-static').onclick = () => addStaticRow('', '');

document.getElementById('csv-input').addEventListener('input', updateCSVPreview);
function updateCSVPreview() {
  const text = document.getElementById('csv-input').value.trim();
  const el = document.getElementById('csv-preview');
  if (!text) { el.textContent = ''; el.className = ''; return; }
  const lines = text.split('\n').filter(l => l.trim());
  const cols = lines[0] ? lines[0].split(',').map(s => s.trim()).filter(Boolean) : [];
  const slots = lines.length - 1;
  if (slots <= 0) {
    el.textContent = cols.length + ' column' + (cols.length === 1 ? '' : 's') + ' — add data rows below the header';
    el.className = 'warn'; return;
  }
  el.textContent = cols.length + ' variable' + (cols.length === 1 ? '' : 's') + ' × ' + slots + ' student slot' + (slots === 1 ? '' : 's') + ' detected';
  el.className = 'ok';
}

document.getElementById('btn-save').onclick = async () => {
  const btn = document.getElementById('btn-save');
  const errEl = document.getElementById('save-error');
  errEl.textContent = '';

  const staticVars = [];
  document.querySelectorAll('.static-row').forEach(row => {
    const k = row.querySelector('.key-in').value.trim();
    const v = row.querySelector('.val-in').value.trim();
    if (k) staticVars.push({key: k, value: v});
  });
  const csvText = document.getElementById('csv-input').value;
  const trimmed = csvText.trim();
  const dataLines = trimmed ? trimmed.split('\n').filter(l => l.trim()).length : 0;
  if (staticVars.length === 0 && dataLines <= 1) {
    errEl.textContent = 'Add at least one variable or one CSV data row before opening.';
    return;
  }

  btn.disabled = true; btn.textContent = 'Saving…';
  try {
    const resp = await fetch('/config', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({static_vars: staticVars, csv_text: csvText})
    });
    if (!resp.ok) { errEl.textContent = 'Error: ' + await resp.text(); return; }
    show('v-roster');
  } catch(e) {
    errEl.textContent = 'Save failed: ' + e.message;
  } finally {
    btn.disabled = false; btn.textContent = 'Save & Open for Students';
  }
};

document.getElementById('btn-reset').onclick = async () => {
  const n = appState && appState.students ? appState.students.length : 0;
  const msg = n > 0
    ? 'This will delete all ' + n + ' student assignment' + (n===1?'':'s') + ' and clear the config. Cannot be undone. Continue?'
    : 'Clear config and reset? Cannot be undone.';
  if (!confirm(msg)) return;
  await fetch('/reset', {method:'POST'});
};

document.getElementById('btn-edit').onclick = async () => {
  const n = appState && appState.students ? appState.students.length : 0;
  if (n > 0 && !confirm(n + ' student' + (n === 1 ? '' : 's') + ' already registered. Their assignments are preserved. Continue editing?')) return;
  try {
    const resp = await fetch('/config-data');
    const cfg = await resp.json();
    const container = document.getElementById('static-rows');
    container.innerHTML = '';
    (cfg.static_vars || []).forEach(sv => addStaticRow(sv.key, sv.value));
    if (container.children.length === 0) addStaticRow('', '');
    document.getElementById('csv-input').value = cfg.csv_text || '';
    updateCSVPreview();
    show('v-config');
  } catch(e) {
    alert('Could not load config: ' + e.message);
  }
};

function copyCmd() {
  const cmd = document.getElementById('setup-cmd').textContent;
  const btn = document.getElementById('btn-copy');
  navigator.clipboard.writeText(cmd).then(() => {
    btn.textContent = '✓ Copied';
    btn.style.color = '#68d391';
    setTimeout(() => { btn.textContent = 'Copy'; btn.style.color = ''; }, 1500);
  }).catch(() => { prompt('Copy this command:', cmd); });
}

connect();
</script>
</body>
</html>`
