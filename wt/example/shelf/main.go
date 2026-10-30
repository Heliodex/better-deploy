package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"wt.packages/app"
	"wt.packages/storage"
	wasihttp "wt.packages/wasihttp"
)

const (
	pageSize       = 12
	maxTags        = 8
	maxTitle       = 200
	maxDescription = 600
	maxURL         = 2000
)

const linkColumns = `l.id, l.url, l.title, l.description, l.created_at,
	(select count(*) from clicks c where c.link_id = l.id) as clicks,
	coalesce((select max(c.clicked_at) from clicks c where c.link_id = l.id), '') as last_clicked_at,
	coalesce((select group_concat(t.tag, ',') from tags t where t.link_id = l.id), '') as tags`

const linkFilter = `where (? = '' or instr(lower(l.title), lower(?)) > 0 or instr(lower(l.url), lower(?)) > 0 or instr(lower(l.description), lower(?)) > 0)
	and (? = '' or exists (select 1 from tags t where t.link_id = l.id and t.tag = ?))`

type link struct {
	ID, URL, Title, Description, Host, CreatedAt, LastClickedAt string
	Clicks                                                      int64
	Tags                                                        []string
}

type appStats struct {
	Links, Clicks, Tags int64
}

type tagCount struct {
	Tag   string
	Count int64
	URL   string
}

type click struct {
	At string
}

type dayCount struct {
	Day     string
	Count   int64
	Percent int
}

type indexData struct {
	Title                      string
	Stats                      appStats
	Links                      []link
	Tags                       []tagCount
	Q, Tag                     string
	Page, Pages                int
	Total                      int64
	Added, Deleted             bool
	Error                      string
	TagURL                     func(string) string
	ClearURL, PrevURL, NextURL string
}

type detailData struct {
	Title  string
	Link   link
	Clicks []click
	Days   []dayCount
	TagURL func(string) string
}

type linkInput struct {
	URL, Title, Description string
	Tags                    []string
}

type rowScanner interface {
	Scan(...any) error
}

var (
	store    *storage.Client
	storeErr error
	mux      *http.ServeMux
)

func init() {
	store, storeErr = storage.New(wasihttp.NewClient())

	mux = http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("POST /links", handleCreate)
	mux.HandleFunc("GET /links/{id}", handleDetail)
	mux.HandleFunc("POST /links/{id}/delete", handleDelete)
	mux.HandleFunc("GET /go/{id}", handleGo)

	wasihttp.Handle(app.ServeStorage(storeErr, migrate, mux).ServeHTTP)
}

func main() {}

func seed(ctx context.Context) error {
	return app.SeedOnce(ctx, store, "links", func(ctx context.Context) error {
		now := time.Now().UTC()
		seeds := []struct {
			id, url, title, description string
			tags                        []string
			clicks                      int
		}{
			{"wasmtime", "https://wasmtime.dev", "Wasmtime", "Fast, secure, and configurable WebAssembly runtime.", []string{"wasm", "runtime"}, 6},
			{"wasi", "https://wasi.dev", "WASI", "The WebAssembly System Interface.", []string{"wasm", "wasi"}, 4},
			{"go", "https://go.dev", "The Go Programming Language", "Build fast, reliable, and efficient software.", []string{"go", "language"}, 5},
			{"components", "https://component-model.bytecodealliance.org", "Component Model", "WebAssembly components, explained.", []string{"wasm", "components"}, 3},
			{"sqlite", "https://sqlite.org", "SQLite", "Small, fast, self-contained SQL database.", []string{"sqlite", "database"}, 2},
		}

		statements := make([]storage.Statement, 0, 64)
		for i, seed := range seeds {
			createdAt := now.Add(-time.Duration(i*2) * 24 * time.Hour).Format(app.TimestampFormat)
			statements = append(statements, storage.Statement{
				SQL:  `insert or ignore into links (id, url, title, description, created_at) values (?, ?, ?, ?, ?)`,
				Args: []any{seed.id, seed.url, seed.title, seed.description, createdAt},
			})
			for _, tag := range seed.tags {
				statements = append(statements, storage.Statement{
					SQL:  `insert or ignore into tags (link_id, tag) values (?, ?)`,
					Args: []any{seed.id, tag},
				})
			}
			for n := range seed.clicks {
				clickedAt := now.Add(-time.Duration(((n/2)*3+i)%14) * 24 * time.Hour).Add(time.Duration(n) * time.Hour).Format(app.TimestampFormat)
				statements = append(statements, storage.Statement{
					SQL:  `insert or ignore into clicks (id, link_id, clicked_at) values (?, ?, ?)`,
					Args: []any{fmt.Sprintf("seed-%s-%d", seed.id, n), seed.id, clickedAt},
				})
			}
		}
		_, err := store.Batch(ctx, statements...)
		return err
	})
}

func migrate(ctx context.Context) error {
	_, err := store.Batch(ctx,
		storage.Statement{SQL: `create table if not exists links (
			id text primary key,
			url text not null,
			title text not null,
			description text not null default '',
			created_at text not null
		)`},
		storage.Statement{SQL: `create table if not exists tags (
			link_id text not null,
			tag text not null,
			primary key (link_id, tag)
		)`},
		storage.Statement{SQL: `create table if not exists clicks (
			id text primary key,
			link_id text not null,
			clicked_at text not null
		)`},
		storage.Statement{SQL: `create index if not exists tags_tag_idx on tags(tag)`},
		storage.Statement{SQL: `create index if not exists clicks_link_idx on clicks(link_id, clicked_at)`},
		storage.Statement{SQL: `create index if not exists links_created_idx on links(created_at)`},
	)
	if err != nil {
		return err
	}
	return seed(ctx)
}

func loadStats(ctx context.Context) (appStats, error) {
	var stats appStats
	err := store.QueryRow(ctx, `select
		(select count(*) from links),
		(select count(*) from clicks),
		(select count(distinct tag) from tags)`).Scan(&stats.Links, &stats.Clicks, &stats.Tags)
	return stats, err
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	_, err := store.Batch(r.Context(),
		storage.Statement{SQL: `delete from tags where link_id = ?`, Args: []any{id}},
		storage.Statement{SQL: `delete from clicks where link_id = ?`, Args: []any{id}},
		storage.Statement{SQL: `delete from links where id = ?`, Args: []any{id}},
	)
	if err != nil {
		http.Error(w, "delete link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?deleted=1", http.StatusSeeOther)
}

func handleGo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	var target string
	err := store.QueryRow(r.Context(), `select url from links where id = ?`, id).Scan(&target)
	if errors.Is(err, storage.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "load link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := store.Exec(r.Context(), `insert into clicks (id, link_id, clicked_at) values (?, ?, ?)`,
		app.NewID(), id, time.Now().UTC().Format(app.TimestampFormat)); err != nil {
		http.Error(w, "record click: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func loadClicks(ctx context.Context, id string) ([]click, error) {
	rows, err := store.Query(ctx, `select clicked_at from clicks where link_id = ? order by clicked_at desc limit 30`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	clicks := []click{}
	for rows.Next() {
		var item click
		if err := rows.Scan(&item.At); err != nil {
			return nil, err
		}
		clicks = append(clicks, item)
	}
	return clicks, rows.Err()
}

func loadDays(ctx context.Context, id string) ([]dayCount, error) {
	rows, err := store.Query(ctx, `select substr(clicked_at, 1, 10) as day, count(*) as count
		from clicks where link_id = ? group by day order by day desc limit 14`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	days := []dayCount{}
	for rows.Next() {
		var item dayCount
		if err := rows.Scan(&item.Day, &item.Count); err != nil {
			return nil, err
		}
		days = append(days, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(days)
	var maxCount int64
	for _, day := range days {
		if day.Count > maxCount {
			maxCount = day.Count
		}
	}
	for i := range days {
		if maxCount > 0 {
			days[i].Percent = int(days[i].Count * 100 / maxCount)
		}
		if days[i].Count > 0 && days[i].Percent < 4 {
			days[i].Percent = 4
		}
	}
	return days, nil
}

func normalizeTag(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	return strings.TrimPrefix(tag, "#")
}

func validTag(tag string) bool {
	if len(tag) == 0 || len(tag) > 24 {
		return false
	}
	for i, r := range tag {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(tag)-1:
		default:
			return false
		}
	}
	return true
}

func parseTags(raw string) ([]string, error) {
	seen := map[string]bool{}
	tags := []string{}
	for part := range strings.SplitSeq(raw, ",") {
		tag := normalizeTag(part)
		if tag == "" || seen[tag] {
			continue
		}
		if !validTag(tag) {
			return nil, fmt.Errorf("tag %q must be 1-24 lowercase letters, digits, or dashes", tag)
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	if len(tags) > maxTags {
		return nil, fmt.Errorf("use at most %d tags", maxTags)
	}
	sort.Strings(tags)
	return tags, nil
}

func parseLinkInput(r *http.Request) (linkInput, error) {
	rawURL := strings.TrimSpace(r.FormValue("url"))
	if rawURL == "" {
		return linkInput{}, fmt.Errorf("URL is required")
	}
	if len(rawURL) > maxURL {
		return linkInput{}, fmt.Errorf("URL is too long")
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return linkInput{}, fmt.Errorf("URL must be an http or https address")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = parsed.Host
	}
	if len(title) > maxTitle {
		return linkInput{}, fmt.Errorf("title is too long")
	}
	description := strings.TrimSpace(r.FormValue("description"))
	if len(description) > maxDescription {
		return linkInput{}, fmt.Errorf("description is too long")
	}
	tags, err := parseTags(r.FormValue("tags"))
	if err != nil {
		return linkInput{}, err
	}
	return linkInput{URL: parsed.String(), Title: title, Description: description, Tags: tags}, nil
}

func splitTags(raw string) []string {
	if raw == "" {
		return []string{}
	}
	tags := slices.Collect(strings.SplitSeq(raw, ","))
	for i := range tags {
		tags[i] = strings.TrimSpace(tags[i])
	}
	sort.Strings(tags)
	return tags
}

func scanLink(scanner rowScanner) (link, error) {
	var item link
	var tags string
	if err := scanner.Scan(&item.ID, &item.URL, &item.Title, &item.Description, &item.CreatedAt, &item.Clicks, &item.LastClickedAt, &tags); err != nil {
		return link{}, err
	}
	if parsed, err := url.Parse(item.URL); err == nil {
		item.Host = parsed.Host
	}
	item.Tags = splitTags(tags)
	return item, nil
}

func loadLink(ctx context.Context, id string) (link, error) {
	return scanLink(store.QueryRow(ctx, `select `+linkColumns+` from links l where l.id = ?`, id))
}

func pageURL(q, tag string, page int) string {
	values := url.Values{}
	if q != "" {
		values.Set("q", q)
	}
	if tag != "" {
		values.Set("tag", tag)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if len(values) == 0 {
		return "/"
	}
	return "/?" + values.Encode()
}

func tagPageURL(q, tag string) string {
	return pageURL(q, tag, 0)
}

func loadTags(ctx context.Context, q string) ([]tagCount, error) {
	rows, err := store.Query(ctx, `select tag, count(*) as count from tags group by tag order by count desc, tag asc limit 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tags := []tagCount{}
	for rows.Next() {
		var item tagCount
		if err := rows.Scan(&item.Tag, &item.Count); err != nil {
			return nil, err
		}
		item.URL = tagPageURL(q, item.Tag)
		tags = append(tags, item)
	}
	return tags, rows.Err()
}

func loadIndex(ctx context.Context, q, tag string, page int) (indexData, error) {
	args := []any{q, q, q, q, tag, tag}
	var total int64
	if err := store.QueryRow(ctx, `select count(*) from links l `+linkFilter, args...).Scan(&total); err != nil {
		return indexData{}, err
	}

	pages := max(int((total+pageSize-1)/pageSize), 1)
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	offset := (page - 1) * pageSize

	listArgs := slices.Concat(args, []any{pageSize, offset})
	rows, err := store.Query(ctx, `select `+linkColumns+` from links l `+linkFilter+`
		order by l.created_at desc, l.id desc
		limit ? offset ?`, listArgs...)
	if err != nil {
		return indexData{}, err
	}
	defer rows.Close()

	links := []link{}
	for rows.Next() {
		item, err := scanLink(rows)
		if err != nil {
			return indexData{}, err
		}
		links = append(links, item)
	}
	if err := rows.Err(); err != nil {
		return indexData{}, err
	}

	tags, err := loadTags(ctx, q)
	if err != nil {
		return indexData{}, err
	}
	stats, err := loadStats(ctx)
	if err != nil {
		return indexData{}, err
	}

	data := indexData{
		Title:    "Link Shelf",
		Stats:    stats,
		Links:    links,
		Tags:     tags,
		Q:        q,
		Tag:      tag,
		Page:     page,
		Pages:    pages,
		Total:    total,
		TagURL:   func(value string) string { return tagPageURL(q, value) },
		ClearURL: pageURL(q, "", 1),
	}
	if page > 1 {
		data.PrevURL = pageURL(q, tag, page-1)
	}
	if page < pages {
		data.NextURL = pageURL(q, tag, page+1)
	}
	return data, nil
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tag := normalizeTag(r.URL.Query().Get("tag"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))

	data, err := loadIndex(r.Context(), q, tag, page)
	if err != nil {
		http.Error(w, "load shelf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data.Added = r.URL.Query().Get("added") == "1"
	data.Deleted = r.URL.Query().Get("deleted") == "1"
	render(w, http.StatusOK, indexTmpl, data)
}

func handleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	input, err := parseLinkInput(r)
	if err != nil {
		data, loadErr := loadIndex(r.Context(), "", "", 1)
		if loadErr != nil {
			http.Error(w, "load shelf: "+loadErr.Error(), http.StatusInternalServerError)
			return
		}
		data.Error = err.Error()
		render(w, http.StatusBadRequest, indexTmpl, data)
		return
	}

	id := app.NewID()
	statements := make([]storage.Statement, 0, 1+len(input.Tags))
	statements = append(statements, storage.Statement{
		SQL:  `insert into links (id, url, title, description, created_at) values (?, ?, ?, ?, ?)`,
		Args: []any{id, input.URL, input.Title, input.Description, time.Now().UTC().Format(app.TimestampFormat)},
	})
	for _, tag := range input.Tags {
		statements = append(statements, storage.Statement{
			SQL:  `insert into tags (link_id, tag) values (?, ?)`,
			Args: []any{id, tag},
		})
	}
	if _, err := store.Batch(r.Context(), statements...); err != nil {
		http.Error(w, "add link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?added=1", http.StatusSeeOther)
}

func handleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	item, err := loadLink(r.Context(), id)
	if errors.Is(err, storage.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "load link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	clicks, err := loadClicks(r.Context(), id)
	if err != nil {
		http.Error(w, "load clicks: "+err.Error(), http.StatusInternalServerError)
		return
	}
	days, err := loadDays(r.Context(), id)
	if err != nil {
		http.Error(w, "load days: "+err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, detailTmpl, detailData{
		Title:  item.Title,
		Link:   item,
		Clicks: clicks,
		Days:   days,
		TagURL: func(value string) string { return tagPageURL("", value) },
	})
}
