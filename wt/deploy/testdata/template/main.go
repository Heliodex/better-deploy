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
	wasihttp "wt.packages/wasihttp"
)

const (
	maxBody      = 16 << 10
	defaultLimit = 50
	maxLimit     = 100
)

type note struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

type notesResponse struct {
	Notes []note `json:"notes"`
}

var (
	store    *storage.Client
	storeErr error
)

func migrate(ctx context.Context) error {
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
	return err
}

func handleListNotes(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			app.WriteError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(parsed, maxLimit)
	}

	rows, err := store.Query(r.Context(), `select id, text, created_at from notes order by created_at desc limit ?`, limit)
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

func handleCreateNote(w http.ResponseWriter, r *http.Request) {
	var request contract.CreateNoteRequest
	if !app.ReadJSON(w, r, maxBody, "text", &request) {
		return
	}

	item := note{
		ID:        app.NewID(),
		Text:      strings.TrimSpace(request.Text),
		CreatedAt: time.Now().UTC().Format(app.TimestampFormat),
	}
	if _, err := store.Exec(r.Context(), `insert into notes (id, text, created_at) values (?, ?, ?)`,
		item.ID, item.Text, item.CreatedAt); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusCreated, item)
}

func init() {
	store, storeErr = storage.New(wasihttp.NewClient())
	if storeErr != nil {
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/notes", handleListNotes)
	mux.HandleFunc("POST /api/notes", handleCreateNote)

	wasihttp.Handle(app.ServeStorage(storeErr, migrate, mux).ServeHTTP)
}

func main() {}
