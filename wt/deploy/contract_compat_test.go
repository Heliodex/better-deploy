package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// compatDoc parses one schema document for classifier tests.
func compatDoc(t *testing.T, body string) map[string]any {
	t.Helper()

	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}

	return doc
}

// diffCompatSchemas diffs two single-file schema sets for classifier tests.
func diffCompatSchemas(t *testing.T, name, oldBody, newBody string) []compatBreak {
	t.Helper()

	baseline := map[string]map[string]any{name: compatDoc(t, oldBody)}
	current := map[string]map[string]any{name: compatDoc(t, newBody)}

	return compatDocs(baseline, current)
}

func TestCompatIdenticalPasses(t *testing.T) {
	body := `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"title":"T"}`

	breaks := diffCompatSchemas(t, "t.json", body, body)

	if len(breaks) != 0 {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatIgnoresMetadata(t *testing.T) {
	oldBody := `{"type":"object","title":"Old","description":"v1","$id":"a","properties":{}}`
	newBody := `{"type":"object","title":"New","description":"v2","$id":"b","properties":{}}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 0 {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatAddedOptionalPasses(t *testing.T) {
	oldBody := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	newBody := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 0 {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatAddedRequiredFails(t *testing.T) {
	oldBody := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	newBody := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a","b"]}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 1 || breaks[0].Rule != "field added required" || breaks[0].Path != "b" {
		t.Errorf("breaks = %v, want one added-required report", breaks)
	}
}

func TestCompatRemovedFieldFails(t *testing.T) {
	oldBody := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`
	newBody := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 1 || breaks[0].Rule != "field removed" || breaks[0].Path != "b" {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatRenameReportsOnce(t *testing.T) {
	oldBody := `{"type":"object","properties":{"title":{"type":"string","minLength":1}}}`
	newBody := `{"type":"object","properties":{"name":{"type":"string","minLength":1}}}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 1 || breaks[0].Rule != "field renamed" {
		t.Errorf("breaks = %v, want one rename", breaks)
	}

	if !strings.Contains(breaks[0].Detail, "title → name") {
		t.Errorf("detail = %q", breaks[0].Detail)
	}
}

func TestCompatTypeChangeFails(t *testing.T) {
	oldBody := `{"type":"object","properties":{"n":{"type":"integer"}}}`
	newBody := `{"type":"object","properties":{"n":{"type":"string"}}}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 1 || breaks[0].Rule != "type changed" || breaks[0].Path != "n" {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatNullability(t *testing.T) {
	adding := []string{
		`{"type":"object","properties":{"n":{"type":"string"}}}`,
		`{"type":"object","properties":{"n":{"type":["string","null"]}}}`,
	}

	breaks := diffCompatSchemas(t, "t.json", adding[0], adding[1])

	if len(breaks) != 1 || breaks[0].Rule != "now nullable" {
		t.Errorf("adding null breaks = %v", breaks)
	}

	breaks = diffCompatSchemas(t, "t.json", adding[1], adding[0])

	if len(breaks) != 0 {
		t.Errorf("removing null breaks = %v", breaks)
	}
}

func TestCompatConstraintsBothDirectionsFail(t *testing.T) {
	pairs := [][2]string{
		{`{"type":"integer","minimum":1}`, `{"type":"integer","minimum":5}`},
		{`{"type":"integer","minimum":5}`, `{"type":"integer","minimum":1}`},
		{`{"type":"string","minLength":1}`, `{"type":"string"}`},
		{`{"type":"string"}`, `{"type":"string","minLength":1}`},
		{`{"type":"array","items":{"type":"string"}}`, `{"type":"array","items":{"type":"integer"}}`},
	}

	for _, pair := range pairs {
		breaks := diffCompatSchemas(t, "t.json", pair[0], pair[1])

		if len(breaks) == 0 {
			t.Errorf("%s → %s should break", pair[0], pair[1])
		}
	}
}

func TestCompatEnumAndPatternFail(t *testing.T) {
	oldBody := `{"type":"object","properties":{"d":{"type":"string","pattern":"^(a|b)$"}}}`
	newBody := `{"type":"object","properties":{"d":{"type":"string","pattern":"^(a|b|c)$"}}}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 1 || !strings.Contains(breaks[0].Rule, "pattern") {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatCompositionKeywordChangeFails(t *testing.T) {
	body := `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}`

	if breaks := diffCompatSchemas(t, "t.json", body, body); len(breaks) != 0 {
		t.Errorf("unchanged composition breaks = %v", breaks)
	}

	changed := `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"boolean"}]}}}`

	breaks := diffCompatSchemas(t, "t.json", body, changed)
	if len(breaks) != 1 || !strings.Contains(breaks[0].Rule, "anyOf") {
		t.Errorf("changed composition breaks = %v", breaks)
	}
}

func TestCompatConstraintRuleNamesKeyword(t *testing.T) {
	oldBody := `{"type":"string","minLength":1}`
	newBody := `{"type":"string","minLength":3}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 1 || breaks[0].Rule != "keyword minLength changed" {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatRequiredFlipsFail(t *testing.T) {
	oldBody := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	newBody := `{"type":"object","properties":{"a":{"type":"string"}}}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 1 || breaks[0].Rule != "now optional" {
		t.Errorf("required→optional breaks = %v", breaks)
	}

	breaks = diffCompatSchemas(t, "t.json", newBody, oldBody)

	if len(breaks) != 1 || breaks[0].Rule != "now required" {
		t.Errorf("optional→required breaks = %v", breaks)
	}
}

func TestCompatReportsNestedPaths(t *testing.T) {
	oldBody := `{"type":"object","properties":{"counts":{"type":"object","properties":{"ready":{"type":"integer"}},"required":["ready"]}}}`
	newBody := `{"type":"object","properties":{"counts":{"type":"object","properties":{"ready":{"type":"integer"},"fast":{"type":"integer"}},"required":["ready","fast"]}}}`

	breaks := diffCompatSchemas(t, "queue.json", oldBody, newBody)

	if len(breaks) != 1 || breaks[0].Path != "counts.fast" || breaks[0].Rule != "field added required" {
		t.Fatalf("breaks = %v", breaks)
	}

	if !strings.Contains(breaks[0].String(), "queue.json counts.fast") {
		t.Errorf("report = %q", breaks[0].String())
	}
}

func TestCompatSameRefPasses(t *testing.T) {
	ref := `{"$ref":"shared.json"}`
	shared := `{"type":"object","properties":{"a":{"type":"string"}}}`

	baseline := map[string]map[string]any{"t.json": compatDoc(t, ref), "shared.json": compatDoc(t, shared)}
	current := map[string]map[string]any{"t.json": compatDoc(t, ref), "shared.json": compatDoc(t, shared)}

	breaks := compatDocs(baseline, current)

	if len(breaks) != 0 {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatChangedRefTargetFails(t *testing.T) {
	baseline := map[string]map[string]any{
		"t.json": compatDoc(t, `{"$ref":"a.json"}`),
		"a.json": compatDoc(t, `{"type":"object","properties":{"x":{"type":"string"}}}`),
	}
	current := map[string]map[string]any{
		"t.json": compatDoc(t, `{"$ref":"b.json"}`),
		"b.json": compatDoc(t, `{"type":"object","properties":{"x":{"type":"integer"}}}`),
	}

	breaks := compatDocs(baseline, current)

	if len(breaks) == 0 {
		t.Error("changed reference target should break")
	}
}

func TestCompatDanglingRefFails(t *testing.T) {
	baseline := map[string]map[string]any{"t.json": compatDoc(t, `{"$ref":"missing.json"}`)}
	current := map[string]map[string]any{"t.json": compatDoc(t, `{"type":"object","properties":{}}`)}

	breaks := compatDocs(baseline, current)

	if len(breaks) != 1 || breaks[0].Rule != "reference unresolvable" {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatRefInlineEquivalentPasses(t *testing.T) {
	shared := `{"type":"object","properties":{"a":{"type":"string"}}}`
	inline := `{"type":"object","properties":{"a":{"type":"string"}}}`

	baseline := map[string]map[string]any{"t.json": compatDoc(t, `{"$ref":"shared.json"}`), "shared.json": compatDoc(t, shared)}
	current := map[string]map[string]any{"t.json": compatDoc(t, inline), "shared.json": compatDoc(t, shared)}

	breaks := compatDocs(baseline, current)

	if len(breaks) != 0 {
		t.Errorf("breaks = %v", breaks)
	}
}

func TestCompatSchemaRemovedFailsAddedPasses(t *testing.T) {
	doc := compatDoc(t, `{"type":"object","properties":{}}`)

	breaks := compatDocs(map[string]map[string]any{"gone.json": doc}, map[string]map[string]any{})

	if len(breaks) != 1 || breaks[0].Rule != "schema removed" {
		t.Errorf("removed schema breaks = %v", breaks)
	}

	breaks = compatDocs(map[string]map[string]any{}, map[string]map[string]any{"fresh.json": doc})

	if len(breaks) != 0 {
		t.Errorf("added schema breaks = %v", breaks)
	}
}

func TestCompatUnknownKeywordFails(t *testing.T) {
	oldBody := `{"type":"object","properties":{}}`
	newBody := `{"type":"object","properties":{},"customValidation":{"strict":true}}`

	breaks := diffCompatSchemas(t, "t.json", oldBody, newBody)

	if len(breaks) != 1 || !strings.Contains(breaks[0].Rule, "customValidation") {
		t.Errorf("breaks = %v", breaks)
	}
}

// gitCommitAll initializes dir as a repository and commits everything in it.
func gitCommitAll(t *testing.T, dir string) {
	t.Helper()

	git := func(args ...string) {
		t.Helper()

		cmd := append([]string{"-C", dir, "-c", "user.email=test@example.com", "-c", "user.name=test"}, args...)
		if out, err := exec.Command("git", cmd...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	git("init", "-q")
	git("add", ".")
	git("commit", "--allow-empty", "-qm", "fixtures")
}

func TestCompatSourceAtRefReadsSubdirApp(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "apps", "note")
	lib := filepath.Join(app, "web", "src", "lib")

	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(lib, "contract.ts"), []byte("export const note = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gitCommitAll(t, root)

	source, ok, err := compatSourceAtRef(app, "HEAD", "web")
	if err != nil || !ok {
		t.Fatalf("compatSourceAtRef() = %q, %v, %v", source, ok, err)
	}

	if !strings.Contains(source, "export const note = 1") {
		t.Errorf("source = %q", source)
	}
}

func TestCompatSourceAtRefMissingApp(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "apps", "note")

	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "README"), []byte("no app yet\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gitCommitAll(t, root)

	_, ok, err := compatSourceAtRef(app, "HEAD", "web")
	if err != nil {
		t.Fatal(err)
	}

	if ok {
		t.Error("missing contract source should report no baseline")
	}
}

func TestCompatSourceAtRefUnknownRef(t *testing.T) {
	root := t.TempDir()
	gitCommitAll(t, root)

	if _, _, err := compatSourceAtRef(root, "missing", "web"); err == nil {
		t.Error("unknown ref should fail")
	}
}
