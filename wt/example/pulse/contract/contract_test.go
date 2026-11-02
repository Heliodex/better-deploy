package contract

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestMonitorOmitsEmptyLast(t *testing.T) {
	data, err := json.Marshal(Monitor{
		CreatedAt: "2026-11-02T00:00:00.000000000Z",
		History:   []Check{},
		ID:        "one",
		Name:      "One",
		URL:       "https://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), `"last"`) {
		t.Errorf("empty last was not omitted: %s", data)
	}
}
