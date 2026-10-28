package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"wt.example/starter/contract"
	"wt.packages/storage"
	wasihttp "wt.packages/wasihttp"
)

const (
	timeFormat   = "2006-01-02T15:04:05.000000000Z07:00"
	maxBody      = 16 << 10
	defaultLimit = 50
	maxLimit     = 100
)

//go:embed all:static
var staticFiles embed.FS

type note struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

type notesResponse struct {
	Notes []note `json:"notes"`
}

var (
	store       *storage.Client
	storeErr    error
	schemaMu    sync.Mutex
	schemaReady bool
)

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	data, _ := json.Marshal(value)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func lowerFieldName(message string) string {
	const prefix = "field "
	if !strings.HasPrefix(message, prefix) {
		return message
	}
	rest := message[len(prefix):]
	if rest == "" {
		return message
	}
	return prefix + strings.ToLower(rest[:1]) + rest[1:]
}

// requestError trims the encoding/json/v2 wrapper from a generated validation error, leaving the field and the rule that failed.
func requestError(err error) string {
	message := err.Error()
	if index := strings.Index(message, ": field "); index >= 0 {
		return lowerFieldName(message[index+2:])
	}
	if index := strings.Index(message, ": invalid value"); index >= 0 {
		return "field text: " + message[index+2:]
	}
	if _, after, ok := strings.CutLast(message, ": "); ok {
		return after
	}
	return message
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.UnmarshalRead(r.Body, out); err != nil {
		writeError(w, http.StatusBadRequest, requestError(err))
		return false
	}
	return true
}

func ensureSchema(ctx context.Context) error {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	if schemaReady {
		return nil
	}
	_, err := store.Exec(ctx, `create table if not exists notes (
		id text primary key,
		text text not null,
		created_at text not null
	)`)
	if err != nil {
		return err
	}
	_, err = store.Exec(ctx, `create index if not exists notes_created_idx on notes(created_at)`)
	if err != nil {
		return err
	}
	schemaReady = true
	return nil
}

func handleListNotes(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(parsed, maxLimit)
	}

	rows, err := store.Query(r.Context(), `select id, text, created_at from notes order by created_at desc limit ?`, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []note{}
	for rows.Next() {
		var item note
		if err := rows.Scan(&item.ID, &item.Text, &item.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, notesResponse{Notes: out})
}

func handleCreateNote(w http.ResponseWriter, r *http.Request) {
	var request contract.CreateNoteRequest
	if !readJSON(w, r, &request) {
		return
	}

	item := note{
		ID:        newID(),
		Text:      strings.TrimSpace(request.Text),
		CreatedAt: time.Now().UTC().Format(timeFormat),
	}
	if _, err := store.Exec(r.Context(), `insert into notes (id, text, created_at) values (?, ?, ?)`,
		item.ID, item.Text, item.CreatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func init() {
	store, storeErr = storage.New(wasihttp.NewClient())
	if storeErr != nil {
		return
	}

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/notes", handleListNotes)
	mux.HandleFunc("POST /api/notes", handleCreateNote)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_app/") {
			w.Header().Set("cache-control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	}))

	wasihttp.Handle(func(w http.ResponseWriter, r *http.Request) {
		if storeErr != nil {
			http.Error(w, "storage unavailable: "+storeErr.Error(), http.StatusServiceUnavailable)
			return
		}
		if err := ensureSchema(r.Context()); err != nil {
			http.Error(w, "storage schema: "+err.Error(), http.StatusInternalServerError)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func main() {}
