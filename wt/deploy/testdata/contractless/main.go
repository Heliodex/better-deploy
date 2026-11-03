package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/heliodex/better-deploy/wt/packages/app"
	"github.com/heliodex/better-deploy/wt/packages/storage"
)

const (
	maxBody      = 16 << 10
	maxNoteText  = 280
	defaultLimit = 50
	maxLimit     = 100
)

// note is the hand-written response shape. Keep it in step with web/src/lib/contract.ts.
type note struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

type notesResponse struct {
	Notes []note `json:"notes"`
}

type createNoteRequest struct {
	Text string `json:"text"`
}

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

	out := []note{}

	for rows.Next() {
		var item note
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

	app.WriteJSON(w, http.StatusOK, notesResponse{Notes: out})
}

func (a *notesApp) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	var request createNoteRequest
	if !app.ReadJSON(w, r, maxBody, "text", &request) {
		return
	}

	text := strings.TrimSpace(request.Text)
	if text == "" {
		app.WriteErrorField(w, http.StatusBadRequest, "invalid", "text", "Please write something.")
		return
	}

	if utf8.RuneCountInString(text) > maxNoteText {
		app.WriteErrorField(w, http.StatusBadRequest, "invalid", "text", "Notes are limited to 280 characters.")
		return
	}

	item := note{
		ID:        app.NewID(),
		Text:      text,
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
