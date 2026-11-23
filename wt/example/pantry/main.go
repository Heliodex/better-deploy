package main

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/heliodex/better-deploy/wt/packages/v2/app"
	"github.com/heliodex/better-deploy/wt/packages/v2/storage"
	wasihttp "github.com/heliodex/better-deploy/wt/packages/v2/wasihttp"
)

type (
	categoryOption struct {
		Key, Label string
	}

	pantryApp struct {
		store *storage.Client
	}

	pantryItem struct {
		ID, Name, Category, CategoryLabel, Unit string
		Quantity, LowAt int64
		Low bool
		Updated string
	}

	categoryGroup struct {
		Key, Label string
		Items []pantryItem
	}

	pantryView struct {
		Groups []categoryGroup
		Total, LowCount int
		Q, Filter, Error string
		Categories []categoryOption
	}

	pageData struct {
		Pantry pantryView
	}
)

const (
	hoursPerDay = 24
	maxFormBytes = 16 << 10
	maxItems = 500
	maxName = 80
	maxQuantity = 9999
	maxUnit = 16
)

var (
	categoryOptions = []categoryOption{
		{
			Key: "dry",
			Label: "Dry goods",
		},
		{
			Key: "fridge",
			Label: "Fridge",
		},
		{
			Key: "freezer",
			Label: "Freezer",
		},
		{
			Key: "other",
			Label: "Other",
		},
	}

	//go:embed templates/*.html
	templateFiles embed.FS
	templates = template.Must(template.ParseFS(templateFiles, "templates/*.html"))
)

func categoryLabel(value string) string {
	for _, option := range categoryOptions {
		if value == option.Key {
			return option.Label
		}
	}

	return "Other"
}

func formInt(value string, fallback int64) (int64, error) {
	value = strings.TrimSpace(value)

	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 || parsed > maxQuantity {
		return 0, fmt.Errorf("invalid number %q", value)
	}

	return parsed, nil
}

func formValue(r *http.Request, key string) string {
	if r.PostForm == nil {
		return ""
	}

	return r.PostForm.Get(key)
}

func main() {}

func migrate(ctx context.Context, store *storage.Client) error {
	return store.Migrate(ctx, 1, storage.Statement{
		SQL: `create table if not exists pantry_items (
				id text primary key,
				name text not null,
				category text not null,
				quantity integer not null,
				unit text not null,
				low_at integer not null,
				updated_at text not null
			)`,
	}, storage.Statement{
		SQL: `create index if not exists pantry_items_shelf_idx on pantry_items(category, name)`,
	})
}

// newPantryApp builds the app around one storage client so tests can use an in-memory service.
func newPantryApp(store *storage.Client) *pantryApp {
	return &pantryApp{
		store: store,
	}
}

func (a *pantryApp) render(w http.ResponseWriter, r *http.Request, view pantryView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	name := "page"
	data := any(pageData{
		Pantry: view,
	})

	if r.Header.Get("Hx-Request") == "true" {
		name = "pantry"
		data = view
	}

	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
	}
}

func relativeTime(value string) string {
	at, err := time.Parse(app.TimestampFormat, value)
	if err != nil {
		return "unknown"
	}

	age := time.Since(at)

	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	}

	return fmt.Sprintf("%dd ago", int(age.Hours()/hoursPerDay))
}

// loadView normalises the search and low-stock filter, loads up to maxItems rows and groups them by shelf.
func (a *pantryApp) loadView(ctx context.Context, q, filter string) (pantryView, error) {
	view := pantryView{
		Q: strings.TrimSpace(q),
		Filter: strings.ToLower(strings.TrimSpace(filter)),
		Categories: categoryOptions,
	}
	if len(view.Q) > 60 {
		view.Q = view.Q[:60]
	}

	if view.Filter != "low" {
		view.Filter = "all"
	}

	rows, err := a.store.QueryN(ctx, maxItems, `select id, name, category, quantity, unit, low_at, updated_at
		from pantry_items
		where (? = '' or instr(lower(name), lower(?)) > 0)
		and (? != 'low' or quantity <= low_at)
		order by case when quantity <= low_at then 0 else 1 end, category, name collate nocase`, view.Q, view.Q, view.Filter)
	if err != nil {
		return view, err
	}

	defer rows.Close()

	groups := make(map[string]*categoryGroup, len(categoryOptions))
	order := make([]*categoryGroup, 0, len(categoryOptions))

	for _, option := range categoryOptions {
		group := &categoryGroup{
			Key: option.Key,
			Label: option.Label,
		}
		groups[option.Key] = group
		order = append(order, group)
	}

	for rows.Next() {
		var (
			item pantryItem
			updated string
		)

		if err := rows.Scan(&item.ID, &item.Name, &item.Category, &item.Quantity, &item.Unit, &item.LowAt, &updated); err != nil {
			return view, err
		}

		item.CategoryLabel = categoryLabel(item.Category)
		item.Low = item.Quantity <= item.LowAt
		item.Updated = relativeTime(updated)
		group := groups[item.Category]

		if group == nil {
			group = groups["other"]
			item.CategoryLabel = "Other"
		}

		group.Items = append(group.Items, item)
		view.Total++

		if item.Low {
			view.LowCount++
		}
	}

	if err := rows.Err(); err != nil {
		return view, err
	}

	for _, group := range order {
		if len(group.Items) > 0 {
			view.Groups = append(view.Groups, *group)
		}
	}

	return view, nil
}

// handleIndex renders the full page; /items renders only the fragment HTMX swaps into #pantry.
func (a *pantryApp) handleIndex(w http.ResponseWriter, r *http.Request) {
	view, err := a.loadView(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("filter"))
	if err != nil {
		view.Error = "Could not load the pantry: " + err.Error()
	}

	a.render(w, r, view)
}

func (a *pantryApp) handleItems(w http.ResponseWriter, r *http.Request) {
	view, err := a.loadView(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("filter"))
	if err != nil {
		view.Error = "Could not load the pantry: " + err.Error()
	}

	a.render(w, r, view)
}

func (a *pantryApp) renderError(w http.ResponseWriter, r *http.Request, q, filter, message string) {
	w.Header().Set("X-Pantry-Error", "1")

	view, err := a.loadView(r.Context(), q, filter)
	if err != nil {
		message += " (could not reload the pantry: " + err.Error() + ")"
	}

	view.Error = message

	a.render(w, r, view)
}

func (a *pantryApp) handleAdjust(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)

	if err := r.ParseForm(); err != nil {
		a.renderError(w, r, "", "all", "Could not read the form: "+err.Error())
		return
	}

	delta, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("delta")))
	if err != nil || delta != -1 && delta != 1 {
		a.renderError(w, r, formValue(r, "q"), formValue(r, "filter"), "That quantity change is not valid")
		return
	}

	result, err := a.store.Exec(r.Context(), `update pantry_items set quantity = max(0, quantity + ?), updated_at = ? where id = ?`, delta, time.Now().UTC().Format(app.TimestampFormat), r.PathValue("id"))
	if err != nil {
		a.renderError(w, r, formValue(r, "q"), formValue(r, "filter"), "Could not update that item: "+err.Error())
		return
	}

	if result.RowsAffected == 0 {
		a.renderError(w, r, formValue(r, "q"), formValue(r, "filter"), "That item is no longer on the shelf")
		return
	}

	view, err := a.loadView(r.Context(), formValue(r, "q"), formValue(r, "filter"))
	if err != nil {
		view.Error = "Updated, but could not reload the pantry: " + err.Error()
	}

	a.render(w, r, view)
}

func (a *pantryApp) handleDelete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)

	if err := r.ParseForm(); err != nil {
		a.renderError(w, r, "", "all", "Could not read the form: "+err.Error())
		return
	}

	result, err := a.store.Exec(r.Context(), `delete from pantry_items where id = ?`, r.PathValue("id"))
	if err != nil {
		a.renderError(w, r, formValue(r, "q"), formValue(r, "filter"), "Could not remove that item: "+err.Error())
		return
	}

	if result.RowsAffected == 0 {
		a.renderError(w, r, formValue(r, "q"), formValue(r, "filter"), "That item is already gone")
		return
	}

	view, err := a.loadView(r.Context(), formValue(r, "q"), formValue(r, "filter"))
	if err != nil {
		view.Error = "Removed, but could not reload the pantry: " + err.Error()
	}

	a.render(w, r, view)
}

func validCategory(value string) bool {
	for _, option := range categoryOptions {
		if value == option.Key {
			return true
		}
	}

	return false
}

func (a *pantryApp) handleAdd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)

	if err := r.ParseForm(); err != nil {
		a.renderError(w, r, "", "all", "Could not read the form: "+err.Error())
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	category := strings.TrimSpace(r.PostFormValue("category"))
	unit := strings.TrimSpace(r.PostFormValue("unit"))

	quantity, err := formInt(r.PostFormValue("quantity"), 1)
	if err != nil {
		a.renderError(w, r, "", "all", "Quantity must be a whole number between 0 and 9999")
		return
	}

	lowAt, err := formInt(r.PostFormValue("low_at"), 1)
	if err != nil {
		a.renderError(w, r, "", "all", "Low-water mark must be a whole number between 0 and 9999")
		return
	}

	switch {
	case name == "":
		a.renderError(w, r, "", "all", "Give the item a name")
		return
	case len(name) > maxName:
		a.renderError(w, r, "", "all", "Keep the name under 80 characters")
		return
	case strings.ContainsAny(name, "\x00\r\n\t"):
		a.renderError(w, r, "", "all", "The name cannot contain control characters")
		return
	case !validCategory(category):
		a.renderError(w, r, "", "all", "Choose a valid shelf")
		return
	case unit == "":
		unit = "pcs"
	case len(unit) > maxUnit || strings.ContainsAny(unit, "\x00\r\n\t"):
		a.renderError(w, r, "", "all", "Keep the unit under 16 characters")
		return
	}

	if _, err := a.store.Exec(r.Context(), `insert into pantry_items (id, name, category, quantity, unit, low_at, updated_at) values (?, ?, ?, ?, ?, ?, ?)`, app.NewID(), name, category, quantity, unit, lowAt, time.Now().UTC().Format(app.TimestampFormat)); err != nil {
		a.renderError(w, r, "", "all", "Could not add that item: "+err.Error())
		return
	}

	if r.Header.Get("Hx-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	view, err := a.loadView(r.Context(), "", "all")
	if err != nil {
		view.Error = "Saved, but could not reload the pantry: " + err.Error()
	}

	a.render(w, r, view)
}

func (a *pantryApp) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /items", a.handleItems)
	mux.HandleFunc("POST /items", a.handleAdd)
	mux.HandleFunc("POST /items/{id}/adjust", a.handleAdjust)
	mux.HandleFunc("POST /items/{id}/delete", a.handleDelete)

	return mux
}

func init() {
	store, errStorage := storage.New(wasihttp.NewClient())
	application := newPantryApp(store)

	wasihttp.Handle(app.ServeStorage(errStorage, func(ctx context.Context) error {
		return migrate(ctx, store)
	}, application.handler()).ServeHTTP)
}
