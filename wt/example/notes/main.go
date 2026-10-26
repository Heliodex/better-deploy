package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"wt.example/storage"
	wasihttp "wt.example/wasihttp"
)

const (
	notesKey    = "notes"
	maxNotes    = 50
	maxNoteBody = 2 << 10
)

type note struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

var (
	mu       sync.Mutex
	store    *storage.Client
	storeErr error
)

func loadNotes() ([]note, error) {
	data, ok, err := store.Get(notesKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []note{}, nil
	}
	var notes []note
	if err := json.Unmarshal(data, &notes); err != nil {
		return nil, err
	}
	return notes, nil
}

func saveNotes(notes []note) error {
	data, err := json.Marshal(notes)
	if err != nil {
		return err
	}
	return store.Put(notesKey, data)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func handle(w http.ResponseWriter, r *http.Request) {
	if storeErr != nil {
		http.Error(w, "storage unavailable: "+storeErr.Error(), http.StatusServiceUnavailable)
		return
	}

	switch {
	case r.URL.Path == "/" || r.URL.Path == "":
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "notes example")
		fmt.Fprintln(w, "GET /notes")
		fmt.Fprintln(w, "POST /notes (body is the note text)")

	case r.URL.Path == "/notes" && r.Method == http.MethodGet:
		mu.Lock()
		defer mu.Unlock()
		notes, err := loadNotes()
		if err != nil {
			http.Error(w, "load notes: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, notes)

	case r.URL.Path == "/notes" && r.Method == http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxNoteBody))
		if err != nil {
			http.Error(w, "read note", http.StatusBadRequest)
			return
		}
		text := strings.TrimSpace(string(body))
		if text == "" {
			http.Error(w, "note is empty", http.StatusBadRequest)
			return
		}

		mu.Lock()
		defer mu.Unlock()
		notes, err := loadNotes()
		if err != nil {
			http.Error(w, "load notes: "+err.Error(), http.StatusInternalServerError)
			return
		}
		entry := note{ID: newID(), Body: text, CreatedAt: time.Now().UTC()}
		notes = append(notes, entry)
		if len(notes) > maxNotes {
			notes = notes[len(notes)-maxNotes:]
		}
		if err := saveNotes(notes); err != nil {
			http.Error(w, "save notes: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, entry)

	default:
		http.NotFound(w, r)
	}
}

func init() {
	store, storeErr = storage.New(wasihttp.NewClient())
	wasihttp.Handle(handle)
}

func main() {}
