package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// compatBreak is one breaking response-shape change between the baseline contract and the regenerated one.
type compatBreak struct {
	Schema string
	Path   string
	Rule   string
	Detail string
}

func (b compatBreak) String() string {
	where := b.Schema
	if b.Path != "" {
		where += " " + b.Path
	}

	if b.Detail == "" {
		return fmt.Sprintf("breaking: %s: %s", where, b.Rule)
	}

	return fmt.Sprintf("breaking: %s: %s (%s)", where, b.Rule, b.Detail)
}

// compatIgnored lists keywords that never affect validation.
var compatIgnored = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true,
	"$comment": true, "default": true, "examples": true,
	"readOnly": true, "writeOnly": true, "deprecated": true,
}

// compatWalked lists keywords the differ models itself. Every other keyword is compared as a value,
// so an unfamiliar validation keyword still reports a change instead of passing silently.
var compatWalked = map[string]bool{
	"type": true, "properties": true, "required": true, "items": true,
}

// compatDetailMax caps rendered JSON values in reports.
const compatDetailMax = 120

// renderCompatJSON renders one JSON value compactly for reports.
func renderCompatJSON(value any) string {
	if value == nil {
		return "absent"
	}

	data, err := json.Marshal(value)
	if err != nil {
		return "?"
	}

	if len(data) > compatDetailMax {
		return string(data[:compatDetailMax-3]) + "..."
	}

	return string(data)
}

// compatTypeNames normalizes a type keyword to a set of type names.
func compatTypeNames(value any) map[string]bool {
	set := map[string]bool{}

	switch typed := value.(type) {
	case string:
		set[typed] = true
	case []any:
		for _, entry := range typed {
			if name, ok := entry.(string); ok {
				set[name] = true
			}
		}
	}

	return set
}

// diffCompatType compares type keywords. Dropping null is safe; every other change breaks a laggard.
func diffCompatType(schema, path string, oldValue, newValue any) []compatBreak {
	oldSet := compatTypeNames(oldValue)
	newSet := compatTypeNames(newValue)

	if reflect.DeepEqual(oldSet, newSet) {
		return nil
	}

	if len(oldSet) > 0 && len(newSet) > 0 {
		oldNull, newNull := oldSet["null"], newSet["null"]

		delete(oldSet, "null")
		delete(newSet, "null")

		if reflect.DeepEqual(oldSet, newSet) {
			if oldNull && !newNull {
				return nil
			}

			return []compatBreak{{Schema: schema, Path: path, Rule: "now nullable", Detail: "old clients reject null"}}
		}
	}

	return []compatBreak{{Schema: schema, Path: path, Rule: "type changed", Detail: renderCompatJSON(oldValue) + " → " + renderCompatJSON(newValue)}}
}

// compatProps returns the properties map of an object schema, or an empty map.
func compatProps(node map[string]any) map[string]any {
	properties, _ := node["properties"].(map[string]any)
	if properties == nil {
		return map[string]any{}
	}

	return properties
}

// compatRequiredSet returns the required field names of an object schema.
func compatRequiredSet(node map[string]any) map[string]bool {
	set := map[string]bool{}

	required, _ := node["required"].([]any)
	for _, entry := range required {
		if name, ok := entry.(string); ok {
			set[name] = true
		}
	}

	return set
}

// compatPath joins a child name onto a dotted schema path.
func compatPath(path, name string) string {
	if path == "" {
		return name
	}

	return path + "." + name
}

// diffCompatSchema compares two schema nodes and reports every breaking change.
func diffCompatSchema(schema, path string, oldNode, newNode any) []compatBreak {
	oldMap, oldOK := oldNode.(map[string]any)
	newMap, newOK := newNode.(map[string]any)

	if !oldOK || !newOK {
		if reflect.DeepEqual(oldNode, newNode) {
			return nil
		}

		return []compatBreak{{Schema: schema, Path: path, Rule: "shape changed", Detail: renderCompatJSON(oldNode) + " → " + renderCompatJSON(newNode)}}
	}

	return slices.Concat(
		diffCompatType(schema, path, oldMap["type"], newMap["type"]),
		diffCompatProperties(schema, path, oldMap, newMap),
		diffCompatRequired(schema, path, oldMap, newMap),
		diffCompatItems(schema, path, oldMap, newMap),
		diffCompatKeywords(schema, path, oldMap, newMap),
	)
}

// diffCompatProperties compares properties, including the rename hint and nested schemas.
func diffCompatProperties(schema, path string, oldMap, newMap map[string]any) []compatBreak {
	oldProps := compatProps(oldMap)
	newProps := compatProps(newMap)
	newRequired := compatRequiredSet(newMap)

	var added, removed []string

	for name := range newProps {
		if _, ok := oldProps[name]; !ok {
			added = append(added, name)
		}
	}

	for name := range oldProps {
		if _, ok := newProps[name]; !ok {
			removed = append(removed, name)
		}
	}

	slices.Sort(added)
	slices.Sort(removed)

	var breaks []compatBreak

	if len(added) == 1 && len(removed) == 1 && reflect.DeepEqual(oldProps[removed[0]], newProps[added[0]]) {
		breaks = append(breaks, compatBreak{Schema: schema, Path: path, Rule: "field renamed", Detail: removed[0] + " → " + added[0]})
	} else {
		for _, name := range removed {
			breaks = append(breaks, compatBreak{Schema: schema, Path: compatPath(path, name), Rule: "field removed", Detail: "old clients still require it"})
		}

		for _, name := range added {
			if newRequired[name] {
				breaks = append(breaks, compatBreak{Schema: schema, Path: compatPath(path, name), Rule: "field added required", Detail: "old servers omit it"})
			}
		}
	}

	for name, oldChild := range oldProps {
		if newChild, ok := newProps[name]; ok {
			breaks = append(breaks, diffCompatSchema(schema, compatPath(path, name), oldChild, newChild)...)
		}
	}

	return breaks
}

// diffCompatRequired compares required sets. Either direction breaks a laggard.
// Fields the new schema introduces are reported by the property diff, not here.
func diffCompatRequired(schema, path string, oldMap, newMap map[string]any) []compatBreak {
	oldRequired := compatRequiredSet(oldMap)
	newRequired := compatRequiredSet(newMap)
	oldProps := compatProps(oldMap)
	newProps := compatProps(newMap)

	var breaks []compatBreak

	for name := range newRequired {
		if _, existed := oldProps[name]; existed && !oldRequired[name] {
			breaks = append(breaks, compatBreak{Schema: schema, Path: compatPath(path, name), Rule: "now required", Detail: "old servers omit it"})
		}
	}

	for name := range oldRequired {
		if _, kept := newProps[name]; kept && !newRequired[name] {
			breaks = append(breaks, compatBreak{Schema: schema, Path: compatPath(path, name), Rule: "now optional", Detail: "old clients still require it"})
		}
	}

	return breaks
}

// diffCompatItems compares array item schemas.
func diffCompatItems(schema, path string, oldMap, newMap map[string]any) []compatBreak {
	oldItems, oldOK := oldMap["items"]
	newItems, newOK := newMap["items"]

	if !oldOK && !newOK {
		return nil
	}

	if !oldOK || !newOK {
		return []compatBreak{{Schema: schema, Path: path, Rule: "array items changed", Detail: renderCompatJSON(oldItems) + " → " + renderCompatJSON(newItems)}}
	}

	return diffCompatSchema(schema, path+"[]", oldItems, newItems)
}

// diffCompatKeywords compares every keyword the differ does not model, including constraints and
// composition. A change to any of them is reported, which stays safe when the generator grows.
func diffCompatKeywords(schema, path string, oldMap, newMap map[string]any) []compatBreak {
	seen := make(map[string]bool, len(oldMap)+len(newMap))
	for keyword := range oldMap {
		seen[keyword] = true
	}

	for keyword := range newMap {
		seen[keyword] = true
	}

	var breaks []compatBreak

	for _, keyword := range slices.Sorted(maps.Keys(seen)) {
		if compatWalked[keyword] || compatIgnored[keyword] || strings.HasPrefix(keyword, "x-") {
			continue
		}

		oldValue, oldOK := oldMap[keyword]
		newValue, newOK := newMap[keyword]

		if oldOK == newOK && reflect.DeepEqual(oldValue, newValue) {
			continue
		}

		breaks = append(breaks, compatBreak{Schema: schema, Path: path, Rule: "keyword " + keyword + " changed", Detail: renderCompatJSON(oldValue) + " → " + renderCompatJSON(newValue)})
	}

	return breaks
}

// compareCompatBreaks orders reports by schema, path and rule.
func compareCompatBreaks(a, b compatBreak) int {
	if a.Schema != b.Schema {
		return strings.Compare(a.Schema, b.Schema)
	}

	if a.Path != b.Path {
		return strings.Compare(a.Path, b.Path)
	}

	return strings.Compare(a.Rule, b.Rule)
}

// compatDocs compares baseline and regenerated schema documents file by file.
func compatDocs(baseline, current map[string]map[string]any) []compatBreak {
	names := slices.Sorted(maps.Keys(baseline))
	for name := range current {
		if _, ok := baseline[name]; !ok {
			names = append(names, name)
		}
	}

	var breaks []compatBreak

	for _, name := range names {
		oldDoc, oldOK := baseline[name]
		newDoc, newOK := current[name]

		switch {
		case !oldOK:
			continue
		case !newOK:
			breaks = append(breaks, compatBreak{Schema: name, Rule: "schema removed", Detail: "old clients still expect it"})
			continue
		}

		oldInlined, err := inlineCompatRefs(baseline, oldDoc, nil)
		if err != nil {
			breaks = append(breaks, compatBreak{Schema: name, Rule: "reference unresolvable", Detail: err.Error()})
			continue
		}

		newInlined, err := inlineCompatRefs(current, newDoc, nil)
		if err != nil {
			breaks = append(breaks, compatBreak{Schema: name, Rule: "reference unresolvable", Detail: err.Error()})
			continue
		}

		breaks = append(breaks, diffCompatSchema(name, "", oldInlined, newInlined)...)
	}

	slices.SortFunc(breaks, compareCompatBreaks)

	return breaks
}

// inlineCompatRefs replaces sibling-file references with their targets so the differ sees plain trees.
// A recursive reference is left in place; its target document is still compared on its own.
func inlineCompatRefs(docs map[string]map[string]any, node any, stack []string) (any, error) {
	switch typed := node.(type) {
	case map[string]any:
		ref, isRef := typed["$ref"].(string)
		if !isRef {
			out := make(map[string]any, len(typed))

			for key, value := range typed {
				inlined, err := inlineCompatRefs(docs, value, stack)
				if err != nil {
					return nil, err
				}

				out[key] = inlined
			}

			return out, nil
		}

		target, err := resolveCompatRef(docs, ref)
		if err != nil {
			return nil, err
		}

		if slices.Contains(stack, ref) {
			return typed, nil
		}

		return inlineCompatRefs(docs, target, append(stack, ref))
	case []any:
		out := make([]any, len(typed))

		for i, value := range typed {
			inlined, err := inlineCompatRefs(docs, value, stack)
			if err != nil {
				return nil, err
			}

			out[i] = inlined
		}

		return out, nil
	default:
		return node, nil
	}
}

// resolveCompatRef resolves a sibling-file reference like message.json or message.json#/properties/id.
func resolveCompatRef(docs map[string]map[string]any, ref string) (any, error) {
	file, fragment, _ := strings.Cut(ref, "#")

	doc, ok := docs[file]
	if !ok {
		return nil, fmt.Errorf("unresolvable reference %s", ref)
	}

	if fragment == "" {
		return doc, nil
	}

	return walkCompatPointer(doc, fragment)
}

// walkCompatPointer follows a JSON pointer fragment like /properties/counts.
func walkCompatPointer(node any, fragment string) (any, error) {
	current := node

	for part := range strings.SplitSeq(strings.TrimPrefix(fragment, "/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")

		mapped, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reference fragment %s does not resolve", fragment)
		}

		current, ok = mapped[part]
		if !ok {
			return nil, fmt.Errorf("reference fragment %s does not resolve", fragment)
		}
	}

	return current, nil
}

// loadCompatDocs parses every schema document in the generated tree.
func loadCompatDocs(generatedDir string) (map[string]map[string]any, error) {
	files, err := contractSchemaFiles(generatedDir)
	if err != nil {
		return nil, err
	}

	docs := make(map[string]map[string]any, len(files))

	for name, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}

		docs[name] = doc
	}

	return docs, nil
}

// gitWorkTree reports whether dir sits inside a git work tree.
func gitWorkTree(dir string) bool {
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "rev-parse", "--is-inside-work-tree")

	return cmd.Run() == nil
}

// compatRepoPath returns the app directory relative to the repository root, with a trailing slash.
func compatRepoPath(dir string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "rev-parse", "--show-prefix")

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read repo prefix: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// compatSourceAtRef reads the tracked contract module at one ref, and reports false when the app had none.
func compatSourceAtRef(dir, ref, web string) (string, bool, error) {
	verify := exec.CommandContext(context.Background(), "git", "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err := verify.Run(); err != nil {
		return "", false, fmt.Errorf("unknown git ref %s", ref)
	}

	prefix, err := compatRepoPath(dir)
	if err != nil {
		return "", false, err
	}

	path := prefix + filepath.ToSlash(filepath.Join(web, "src", "lib", "contract.ts"))

	exists := exec.CommandContext(context.Background(), "git", "-C", dir, "cat-file", "-e", ref+":"+path)
	if err := exists.Run(); err != nil {
		return "", false, nil
	}

	show := exec.CommandContext(context.Background(), "git", "-C", dir, "show", ref+":"+path)

	data, err := show.Output()
	if err != nil {
		return "", false, fmt.Errorf("read %s at %s: %w", path, ref, err)
	}

	return string(data), true, nil
}

// generateCompatBaseline derives the schema documents at one ref from the tracked contract module,
// so the check works even though generated schemas are ignored by git.
func generateCompatBaseline(dir, web, appName, ref string) (map[string]map[string]any, error) {
	source, ok, err := compatSourceAtRef(dir, ref, web)
	if err != nil || !ok {
		return map[string]map[string]any{}, err
	}

	file, err := os.CreateTemp(filepath.Join(dir, web, "src", "lib"), ".wt-baseline-*.ts")
	if err != nil {
		return nil, err
	}

	sourcePath := file.Name()

	defer os.Remove(sourcePath)

	if _, err := file.WriteString(source); err != nil {
		file.Close()

		return nil, err
	}

	if err := file.Close(); err != nil {
		return nil, err
	}

	outDir, err := os.MkdirTemp(dir, ".wt-compat-baseline-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(outDir)

	module := "./src/lib/" + strings.TrimSuffix(filepath.Base(sourcePath), ".ts")
	if err := runContractSchemas(filepath.Join(dir, web), appName, outDir, module, true); err != nil {
		return nil, err
	}

	return loadCompatDocs(outDir)
}

// runCompat regenerates the contract and reports breaking schema changes against a git ref.
func runCompat(dir, web, appName, ref string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	if !gitWorkTree(abs) {
		fmt.Fprintln(os.Stderr, "compat: skipped (not a git checkout)")

		return nil
	}

	generated, err := os.MkdirTemp(abs, ".wt-compat-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(generated)

	if err := generateContractTo(abs, web, appName, generated, true); err != nil {
		return err
	}

	current, err := loadCompatDocs(generated)
	if err != nil {
		return err
	}

	baseline, err := generateCompatBaseline(abs, web, appName, ref)
	if err != nil {
		return err
	}

	breaks := compatDocs(baseline, current)

	if len(breaks) == 0 {
		fmt.Fprintln(os.Stderr, "compat: no breaking changes")

		return nil
	}

	lines := make([]string, 0, len(breaks))
	for _, item := range breaks {
		lines = append(lines, item.String())
	}

	return fmt.Errorf("%d breaking contract changes:\n%s", len(breaks), strings.Join(lines, "\n"))
}
