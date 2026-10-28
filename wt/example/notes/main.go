package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"wt.packages/app"
	"wt.packages/storage"
	wasihttp "wt.packages/wasihttp"
)

const (
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

func migrate(ctx context.Context) error {
	_, err := store.Exec(ctx, `create table if not exists notes (
		id text primary key,
		body text not null,
		created_at text not null
	)`)
	return err
}

func loadNotes(ctx context.Context) ([]note, error) {
	rows, err := store.Query(ctx, `select id, body, created_at from notes order by created_at asc, id asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	notes := []note{}
	for rows.Next() {
		var entry note
		if err := rows.Scan(&entry.ID, &entry.Body, &entry.CreatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, entry)
	}
	return notes, rows.Err()
}

func addNote(ctx context.Context, body string) (note, error) {
	entry := note{ID: app.NewID(), Body: body, CreatedAt: time.Now().UTC()}
	_, err := store.Batch(ctx,
		storage.Statement{
			SQL:  `insert into notes (id, body, created_at) values (?, ?, ?)`,
			Args: []any{entry.ID, entry.Body, entry.CreatedAt.UTC().Format(app.TimestampFormat)},
		},
		storage.Statement{
			SQL:  `delete from notes where id not in (select id from notes order by created_at desc, id desc limit ?)`,
			Args: []any{maxNotes},
		},
	)
	if err != nil {
		return note{}, err
	}
	return entry, nil
}

func handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/" || r.URL.Path == "":
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "notes example")
		fmt.Fprintln(w, "GET /notes")
		fmt.Fprintln(w, "POST /notes (body is the note text)")

	case r.URL.Path == "/notes" && r.Method == http.MethodGet:
		ctx := r.Context()
		mu.Lock()
		defer mu.Unlock()
		notes, err := loadNotes(ctx)
		if err != nil {
			http.Error(w, "load notes: "+err.Error(), http.StatusInternalServerError)
			return
		}
		app.WriteJSON(w, http.StatusOK, notes)

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

		ctx := r.Context()
		mu.Lock()
		defer mu.Unlock()
		entry, err := addNote(ctx, text)
		if err != nil {
			http.Error(w, "save notes: "+err.Error(), http.StatusInternalServerError)
			return
		}
		app.WriteJSON(w, http.StatusCreated, entry)

	default:
		http.NotFound(w, r)
	}
}

func init() {
	store, storeErr = storage.New(wasihttp.NewClient())
	wasihttp.Handle(app.ServeStorage(storeErr, migrate, http.HandlerFunc(handle)).ServeHTTP)
}

func main() {}
