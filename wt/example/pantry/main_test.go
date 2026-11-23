//go:build !wasip1

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/heliodex/better-deploy/wt/packages/v2/storage"
)

func doForm(t *testing.T, handler http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	var body *strings.Reader

	if form == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(form.Encode())
	}

	request := httptest.NewRequest(http.MethodPost, path, body)

	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	request.Header.Set("Hx-Request", "true")

	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	return recorder
}

func doGet(handler http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)

	request.Header.Set("Hx-Request", "true")

	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	return recorder
}

func itemID(t *testing.T, store *storage.Client, name string) (id string) {
	t.Helper()

	if err := store.QueryRow(context.Background(), `select id from pantry_items where name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}

	return
}

func testPantry(t *testing.T) (*pantryApp, *storage.Client) {
	t.Helper()

	server, err := storage.NewTestServer()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(server.Close)

	store := server.NewClient(server.Client())
	application := newPantryApp(store)

	if err := migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}

	return application, store
}

func TestPantryAdjustsAndFlagsLowStock(t *testing.T) {
	application, store := testPantry(t)
	handler := application.handler()

	doForm(t, handler, "/items", url.Values{
		"category": {"dry"},
		"low_at": {"1"},
		"name": {"Oats"},
		"quantity": {"2"},
		"unit": {"kg"},
	})

	id := itemID(t, store, "Oats")
	recorder := doForm(t, handler, "/items/"+id+"/adjust", url.Values{
		"delta": {"-1"},
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("adjust = %d: %s", recorder.Code, recorder.Body.String())
	}

	if body := recorder.Body.String(); !strings.Contains(body, "1 kg") || !strings.Contains(body, "low") {
		t.Errorf("adjusted item is not low:\n%s", body)
	}

	recorder = doForm(t, handler, "/items/"+id+"/adjust", url.Values{
		"delta": {"-1"},
	})

	if body := recorder.Body.String(); !strings.Contains(body, "0 kg") {
		t.Errorf("quantity did not clamp at zero:\n%s", body)
	}
}

func TestPantryRejectsEmptyNames(t *testing.T) {
	application, store := testPantry(t)
	handler := application.handler()
	recorder := doForm(t, handler, "/items", url.Values{
		"category": {"dry"},
		"name": {"   "},
	})

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Give the item a name") {
		t.Fatalf("empty name = %d: %s", recorder.Code, recorder.Body.String())
	}

	if recorder.Header().Get("X-Pantry-Error") != "1" {
		t.Error("an invalid add did not mark the response so the form stays filled")
	}

	var count int64

	if err := store.QueryRow(context.Background(), `select count(*) from pantry_items`).Scan(&count); err != nil {
		t.Fatal(err)
	}

	if count != 0 {
		t.Errorf("stored %d items after an invalid add", count)
	}
}

func TestPantryRendersAndAddsItems(t *testing.T) {
	application, store := testPantry(t)
	handler := application.handler()
	page := httptest.NewRecorder()

	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))

	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<h1>Pantry</h1>") {
		t.Fatalf("page = %d: %s", page.Code, page.Body.String())
	}

	recorder := doForm(t, handler, "/items", url.Values{
		"category": {"dry"},
		"low_at": {"1"},
		"name": {"Coffee"},
		"quantity": {"2"},
		"unit": {"bags"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("add = %d: %s", recorder.Code, recorder.Body.String())
	}

	body := recorder.Body.String()

	for _, want := range []string{"Coffee", "2 bags", "Dry goods", "top up at 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("add response is missing %q:\n%s", want, body)
		}
	}

	if got := itemID(t, store, "Coffee"); got == "" {
		t.Fatal("Coffee was not stored")
	}
}

func TestPantrySearchesFiltersAndDeletes(t *testing.T) {
	application, store := testPantry(t)
	handler := application.handler()

	doForm(t, handler, "/items", url.Values{
		"category": {"dry"},
		"low_at": {"1"},
		"name": {"Tea"},
		"quantity": {"5"},
		"unit": {"boxes"},
	})
	doForm(t, handler, "/items", url.Values{
		"category": {"fridge"},
		"low_at": {"1"},
		"name": {"Milk"},
		"quantity": {"1"},
		"unit": {"litre"},
	})

	search := doGet(handler, "/items?q=tea")

	if body := search.Body.String(); !strings.Contains(body, "Tea") || strings.Contains(body, "Milk") {
		t.Errorf("search returned the wrong shelves:\n%s", body)
	}

	low := doGet(handler, "/items?filter=low")

	if body := low.Body.String(); !strings.Contains(body, "Milk") || strings.Contains(body, "Tea") {
		t.Errorf("low filter returned the wrong shelves:\n%s", body)
	}

	id := itemID(t, store, "Tea")
	recorder := doForm(t, handler, "/items/"+id+"/delete", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", recorder.Code, recorder.Body.String())
	}

	if body := recorder.Body.String(); strings.Contains(body, "Tea") {
		t.Errorf("deleted item is still rendered:\n%s", body)
	}
}
