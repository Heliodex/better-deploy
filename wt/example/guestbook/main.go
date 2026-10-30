package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wt.example/guestbook/contract"
	"wt.packages/app"
	"wt.packages/storage"
	wasihttp "wt.packages/wasihttp"
)

const (
	maxBody        = 16 << 10
	defaultLimit   = 50
	maxLimit       = 100
	reactionRocket = "rocket"
	reactionHeart  = "heart"
	reactionEyes   = "eyes"
	reactionBrain  = "brain"
)

type reactionCounts struct {
	Rocket int64 `json:"rocket"`
	Heart  int64 `json:"heart"`
	Eyes   int64 `json:"eyes"`
	Brain  int64 `json:"brain"`
}

type message struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Body      string         `json:"body"`
	CreatedAt string         `json:"createdAt"`
	Reactions reactionCounts `json:"reactions"`
}

type messagesResponse struct {
	Messages []message `json:"messages"`
}

type statsResponse struct {
	Messages  int64 `json:"messages"`
	Reactions int64 `json:"reactions"`
}

type reactResponse struct {
	Reaction string `json:"reaction"`
	Count    int64  `json:"count"`
}

var (
	store    *storage.Client
	storeErr error
)

func seed(ctx context.Context) error {
	return app.SeedOnce(ctx, store, "messages", func(ctx context.Context) error {
		now := time.Now().UTC()
		seeds := []struct {
			id, name, body string
			reactions      map[string]int64
		}{
			{
				"ada",
				"Ada",
				"The component model finally makes this feel like one platform.",
				map[string]int64{reactionRocket: 3, reactionHeart: 2},
			},
			{
				"grace",
				"Grace",
				"Static pages, a live Go API, one .wasm. That is a very tidy stack.",
				map[string]int64{reactionRocket: 2, reactionBrain: 3},
			},
			{
				"linus",
				"Linus",
				"I came for the demo and stayed for the SQLite.",
				map[string]int64{reactionEyes: 1, reactionHeart: 1},
			},
			{
				"margaret",
				"Margaret",
				"Shipping a SvelteKit page inside a Wasm component is delightfully strange.",
				map[string]int64{reactionBrain: 2, reactionRocket: 4},
			},
		}

		statements := make([]storage.Statement, 0, 16)
		for i, seed := range seeds {
			createdAt := now.Add(-time.Duration(i*3+1) * time.Hour).Format(app.TimestampFormat)
			statements = append(statements, storage.Statement{
				SQL:  `insert or ignore into messages (id, name, body, created_at) values (?, ?, ?, ?)`,
				Args: []any{seed.id, seed.name, seed.body, createdAt},
			})
			for reaction, count := range seed.reactions {
				statements = append(statements, storage.Statement{
					SQL:  `insert or ignore into reactions (message_id, reaction, count) values (?, ?, ?)`,
					Args: []any{seed.id, reaction, count},
				})
			}
		}
		_, err := store.Batch(ctx, statements...)
		return err
	})
}

func migrate(ctx context.Context) error {
	err := store.Migrate(ctx, 1,
		storage.Statement{SQL: `create table if not exists messages (
			id text primary key,
			name text not null,
			body text not null,
			created_at text not null
		)`},
		storage.Statement{SQL: `create table if not exists reactions (
			message_id text not null,
			reaction text not null,
			count integer not null default 0,
			primary key (message_id, reaction)
		)`},
		storage.Statement{SQL: `create index if not exists messages_created_idx on messages(created_at)`},
	)
	if err != nil {
		return err
	}
	return seed(ctx)
}

func handleListMessages(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			app.WriteError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(parsed, maxLimit)
	}

	rows, err := store.Query(r.Context(), `select
		m.id, m.name, m.body, m.created_at,
		coalesce(sum(case when r.reaction = 'rocket' then r.count end), 0) as rocket,
		coalesce(sum(case when r.reaction = 'heart' then r.count end), 0) as heart,
		coalesce(sum(case when r.reaction = 'eyes' then r.count end), 0) as eyes,
		coalesce(sum(case when r.reaction = 'brain' then r.count end), 0) as brain
		from messages m
		left join reactions r on r.message_id = m.id
		group by m.id
		order by m.created_at desc
		limit ?`, limit)
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []message{}
	for rows.Next() {
		var item message
		if err := rows.Scan(
			&item.ID, &item.Name, &item.Body, &item.CreatedAt,
			&item.Reactions.Rocket, &item.Reactions.Heart,
			&item.Reactions.Eyes, &item.Reactions.Brain,
		); err != nil {
			app.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusOK, messagesResponse{Messages: out})
}

func handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	var request contract.CreateMessageRequest
	if !app.ReadJSON(w, r, maxBody, "reaction", &request) {
		return
	}

	item := message{
		ID:        app.NewID(),
		Name:      strings.TrimSpace(request.Name),
		Body:      strings.TrimSpace(request.Body),
		CreatedAt: time.Now().UTC().Format(app.TimestampFormat),
	}
	if _, err := store.Exec(r.Context(), `insert into messages (id, name, body, created_at) values (?, ?, ?, ?)`,
		item.ID, item.Name, item.Body, item.CreatedAt); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusCreated, item)
}

func handleReact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		app.WriteError(w, http.StatusNotFound, "message not found")
		return
	}
	var request contract.ReactMessageRequest
	if !app.ReadJSON(w, r, maxBody, "reaction", &request) {
		return
	}
	reaction := string(request.Reaction)

	var exists int64
	if err := store.QueryRow(r.Context(), `select count(*) from messages where id = ?`, id).Scan(&exists); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exists == 0 {
		app.WriteError(w, http.StatusNotFound, "message not found")
		return
	}

	if _, err := store.Exec(r.Context(), `insert into reactions (message_id, reaction, count) values (?, ?, 1)
		on conflict(message_id, reaction) do update set count = count + 1`, id, reaction); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var count int64
	if err := store.QueryRow(r.Context(), `select count from reactions where message_id = ? and reaction = ?`,
		id, reaction).Scan(&count); err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusOK, reactResponse{Reaction: reaction, Count: count})
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	var stats statsResponse
	err := store.QueryRow(r.Context(), `select
		(select count(*) from messages),
		(select coalesce(sum(count), 0) from reactions)`).Scan(&stats.Messages, &stats.Reactions)
	if err != nil {
		app.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	app.WriteJSON(w, http.StatusOK, stats)
}

func init() {
	store, storeErr = storage.New(wasihttp.NewClient())
	if storeErr != nil {
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/messages", handleListMessages)
	mux.HandleFunc("POST /api/messages", handleCreateMessage)
	mux.HandleFunc("POST /api/messages/{id}/react", handleReact)
	mux.HandleFunc("GET /api/stats", handleStats)

	wasihttp.Handle(app.ServeStorage(storeErr, migrate, mux).ServeHTTP)
}

func main() {}
