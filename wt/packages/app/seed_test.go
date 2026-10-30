package app

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wt.packages/storage"
)

func TestSeedOnce(t *testing.T) {
	cases := []struct {
		name         string
		seeded, rows int64
		wantSeed     bool
	}{
		{"empty table seeds", 0, 0, true},
		{"existing rows only mark seeded", 0, 3, false},
		{"seeded marker skips everything", 1, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var statements []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				var req struct {
					SQL string `json:"sql"`
				}
				_ = json.Unmarshal(data, &req)
				statements = append(statements, req.SQL)
				w.Header().Set("content-type", "application/json")
				switch {
				case strings.Contains(req.SQL, "from meta"):
					_, _ = fmt.Fprintf(w, `{"columns":["count"],"rows":[[%d]]}`, tc.seeded)
				case strings.Contains(req.SQL, "notes"):
					_, _ = fmt.Fprintf(w, `{"columns":["count"],"rows":[[%d]]}`, tc.rows)
				default:
					_, _ = io.WriteString(w, `{"rows_affected":1}`)
				}
			}))
			defer server.Close()

			t.Setenv("WT_STORAGE_URL", server.URL)
			t.Setenv("WT_STORAGE_TOKEN", "test")
			client, err := storage.New(server.Client())
			if err != nil {
				t.Fatal(err)
			}

			calls := 0
			err = SeedOnce(context.Background(), client, "notes", func(context.Context) error {
				calls++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := calls == 1; got != tc.wantSeed {
				t.Errorf("seed ran = %v, want %v", got, tc.wantSeed)
			}

			marked := false
			for _, statement := range statements {
				if strings.Contains(statement, "insert or ignore into meta") {
					marked = true
				}
			}
			if want := tc.seeded == 0; marked != want {
				t.Errorf("marked seeded = %v, want %v", marked, want)
			}
		})
	}
}
