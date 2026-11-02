package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"__CONTRACT_IMPORT__"
	"wt.packages/app"
	"wt.packages/storage"
)

const (
	maxBody      = 16 << 10
	defaultLimit = 50
	maxLimit     = 100
)

// notesApp owns the store and routes so handler tests can construct it without wasmtime or package globals.
type notesApp struct {
	store *storage.Client
	mux   *http.ServeMux
}

// migrate creates the schema through the app's store.
func (a *notesApp) migrate(ctx context.Context) error {
	_, err := a.store.Exec(ctx, `create table if not exists notes (
		id text primary key,
		text text not null,
		created_at text not null
	)`)
	if err != nil {
		return err
	}
	_, err = a.store.Exec(ctx, `create index if not exists notes_created_idx on notes(created_at)`)
	return err
}

func (a *notesApp) handleListNotes(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			app.WriteError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(parsed, maxLimit)
	}

	rows, err := a.store.Query(r.Context(), `select id, text, created_at from notes order by created_at desc limit ?`, limit)
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []contract.Note{}
	for rows.Next() {
		var item contract.Note
		if err := rows.Scan(&item.ID, &item.Text, &item.CreatedAt); err != nil {
			app.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusOK, contract.NotesResponse{Notes: out})
}

func (a *notesApp) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	var request contract.CreateNoteRequest
	if !app.ReadJSON(w, r, maxBody, "text", &request) {
		return
	}

	item := contract.Note{
		ID:        app.NewID(),
		Text:      strings.TrimSpace(request.Text),
		CreatedAt: time.Now().UTC().Format(app.TimestampFormat),
	}
	if _, err := a.store.Exec(r.Context(), `insert into notes (id, text, created_at) values (?, ?, ?)`,
		item.ID, item.Text, item.CreatedAt); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusCreated, item)
}

// handler exposes the app's routes to the wasm wiring or a test.
func (a *notesApp) handler() http.Handler {
	return a.mux
}

// newNotesApp builds the app around one store and registers its routes.
func newNotesApp(store *storage.Client) *notesApp {
	a := &notesApp{store: store, mux: http.NewServeMux()}
	a.mux.HandleFunc("GET /api/notes", a.handleListNotes)
	a.mux.HandleFunc("POST /api/notes", a.handleCreateNote)
	return a
}

func main() {}
