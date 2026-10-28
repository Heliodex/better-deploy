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

	"wt.example/guestbook/contract"
	"wt.packages/storage"
	wasihttp "wt.packages/wasihttp"
)

const (
	timeFormat     = "2006-01-02T15:04:05.000000000Z07:00"
	maxBody        = 16 << 10
	defaultLimit   = 50
	maxLimit       = 100
	reactionRocket = "rocket"
	reactionHeart  = "heart"
	reactionEyes   = "eyes"
	reactionBrain  = "brain"
)

//go:embed all:static
var staticFiles embed.FS

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
		return "field reaction: " + message[index+2:]
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

func seed(ctx context.Context) error {
	var seeded int64
	if err := store.QueryRow(ctx, `select count(*) from meta where key = 'seeded'`).Scan(&seeded); err != nil {
		return err
	}
	if seeded > 0 {
		return nil
	}
	var count int64
	if err := store.QueryRow(ctx, `select count(*) from messages`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		_, err := store.Exec(ctx, `insert or ignore into meta (key, value) values ('seeded', '1')`)
		return err
	}

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
		createdAt := now.Add(-time.Duration(i*3+1) * time.Hour).Format(timeFormat)
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
	statements = append(statements, storage.Statement{
		SQL: `insert or ignore into meta (key, value) values ('seeded', '1')`,
	})
	_, err := store.Batch(ctx, statements...)
	return err
}

func ensureSchema(ctx context.Context) error {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	if schemaReady {
		return nil
	}
	_, err := store.Batch(ctx,
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
		storage.Statement{SQL: `create table if not exists meta (
			key text primary key,
			value text not null
		)`},
		storage.Statement{SQL: `create index if not exists messages_created_idx on messages(created_at)`},
	)
	if err != nil {
		return err
	}
	if err := seed(ctx); err != nil {
		return err
	}
	schemaReady = true
	return nil
}

func handleListMessages(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
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
		writeError(w, http.StatusInternalServerError, err.Error())
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
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, messagesResponse{Messages: out})
}

func handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	var request contract.CreateMessageRequest
	if !readJSON(w, r, &request) {
		return
	}

	item := message{
		ID:        newID(),
		Name:      strings.TrimSpace(request.Name),
		Body:      strings.TrimSpace(request.Body),
		CreatedAt: time.Now().UTC().Format(timeFormat),
	}
	if _, err := store.Exec(r.Context(), `insert into messages (id, name, body, created_at) values (?, ?, ?, ?)`,
		item.ID, item.Name, item.Body, item.CreatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func handleReact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	var request contract.ReactMessageRequest
	if !readJSON(w, r, &request) {
		return
	}
	reaction := string(request.Reaction)

	var exists int64
	if err := store.QueryRow(r.Context(), `select count(*) from messages where id = ?`, id).Scan(&exists); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}

	if _, err := store.Exec(r.Context(), `insert into reactions (message_id, reaction, count) values (?, ?, 1)
		on conflict(message_id, reaction) do update set count = count + 1`, id, reaction); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var count int64
	if err := store.QueryRow(r.Context(), `select count from reactions where message_id = ? and reaction = ?`,
		id, reaction).Scan(&count); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reactResponse{Reaction: reaction, Count: count})
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	var stats statsResponse
	err := store.QueryRow(r.Context(), `select
		(select count(*) from messages),
		(select coalesce(sum(count), 0) from reactions)`).Scan(&stats.Messages, &stats.Reactions)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
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
	mux.HandleFunc("GET /api/messages", handleListMessages)
	mux.HandleFunc("POST /api/messages", handleCreateMessage)
	mux.HandleFunc("POST /api/messages/{id}/react", handleReact)
	mux.HandleFunc("GET /api/stats", handleStats)
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
