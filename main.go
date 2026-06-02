package main

import (
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

const (
	dbPath = "students.json"
	port   = ":2225"
	script = "generate_env.sh"
)

type Student struct {
	Name          string `json:"name"`
	UUIDDomain    string `json:"uuid_domain"`
	FirstAccessed string `json:"first_accessed"`
	LastAccessed  string `json:"last_accessed"`
	Payload       string `json:"payload"`
}

type DB struct {
	Students []Student `json:"students"`
}

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

	db, err := loadDB()
	if err != nil {
		log.Printf("ERROR loading %s: %v", dbPath, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
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
			return
		}
	}

	log.Printf("CACHE MISS  %s (%s) — running %s", req.Name, req.UUIDDomain, script)
	cmd := exec.Command("bash", script, req.Name, req.UUIDDomain)
	stdout, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			log.Printf("ERROR %s exited non-zero: %s", script, string(exitErr.Stderr))
		} else {
			log.Printf("ERROR running %s: %v", script, err)
		}
		http.Error(w, "configuration generation failed", http.StatusInternalServerError)
		return
	}

	student := Student{
		Name:          req.Name,
		UUIDDomain:    req.UUIDDomain,
		FirstAccessed: now,
		LastAccessed:  now,
		Payload:       string(stdout),
	}
	db.Students = append(db.Students, student)
	if err := saveDB(db); err != nil {
		log.Printf("ERROR saving %s: %v", dbPath, err)
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, string(stdout))
}

func handleRoster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	db, err := loadDB()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	if len(db.Students) == 0 {
		fmt.Fprintln(w, "No students registered yet.")
		return
	}
	fmt.Fprintf(w, "%-25s  %-50s  %s\n", "NAME", "UUID DOMAIN", "REGISTERED")
	fmt.Fprintln(w, strings.Repeat("-", 90))
	for _, s := range db.Students {
		fmt.Fprintf(w, "%-25s  %-50s  %s\n", s.Name, s.UUIDDomain, s.FirstAccessed)
	}
}

func main() {
	http.HandleFunc("/get-config", handleGetConfig)
	http.HandleFunc("/roster", handleRoster)
	log.Printf("PCDS listening on %s", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
