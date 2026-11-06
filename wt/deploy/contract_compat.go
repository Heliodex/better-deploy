package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// compatConstraintKeywords are JSON Schema keywords that constrain values. Any change to one
// breaks whichever side lags: tightening breaks new clients against old servers, loosening
// breaks old clients against new servers.
var compatConstraintKeywords = []string{
	"minLength", "maxLength", "minimum", "maximum",
	"exclusiveMinimum", "exclusiveMaximum", "minItems", "maxItems",
	"minProperties", "maxProperties", "multipleOf", "uniqueItems",
	"pattern", "format", "const", "enum", "additionalProperties",
}

// compatMetadataKeywords never affect validation and are ignored by the comparison.
var compatMetadataKeywords = []string{
	"title", "$id", "$schema", "$comment", "description", "default",
	"examples", "readOnly", "writeOnly", "deprecated",
}

// compatDocs compares baseline and regenerated schema documents file by file.
func compatDocs(baseline, current map[string]map[string]any) []compatBreak {
	names := make([]string, 0, len(baseline)+len(current))
	seen := map[string]bool{}

	for name := range baseline {
		seen[name] = true
		names = append(names, name)
	}

	for name := range current {
		if !seen[name] {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	resolver := &compatResolver{baseline: baseline, current: current}

	var breaks []compatBreak

	for _, name := range names {
		oldDoc, oldOK := baseline[name]
		newDoc, newOK := current[name]

		switch {
		case !oldOK:
			continue
		case !newOK:
			breaks = append(breaks, compatBreak{Schema: name, Rule: "schema removed", Detail: "old clients still expect it"})
		default:
			breaks = append(breaks, diffCompatValue(name, "", oldDoc, newDoc, "baseline", "current", resolver)...)
		}
	}

	return breaks
}

// compatResolver resolves sibling-file $refs on either side of the comparison.
type compatResolver struct {
	baseline map[string]map[string]any
	current  map[string]map[string]any
	stack    []string
}

func (r *compatResolver) resolve(side, ref string) (map[string]any, error) {
	file, _, _ := strings.Cut(ref, "#")

	key := side + "\x00" + ref
	if slices.Contains(r.stack, key) {
		return nil, fmt.Errorf("reference cycle at %s", ref)
	}

	var docs map[string]map[string]any

	if side == "baseline" {
		docs = r.baseline
	} else {
		docs = r.current
	}

	doc, ok := docs[file]
	if !ok {
		return nil, fmt.Errorf("unresolvable reference %s", ref)
	}

	node := doc

	if _, fragment, ok := strings.Cut(ref, "#"); ok {
		walked, err := walkCompatPointer(node, fragment)
		if err != nil {
			return nil, err
		}

		mapped, ok := walked.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reference %s does not point at a schema", ref)
		}

		node = mapped
	}

	r.stack = append(r.stack, key)
	defer func() { r.stack = r.stack[:len(r.stack)-1] }()

	return node, nil
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

// compatRefOf returns the $ref target when the node is a reference.
func compatRefOf(node any) (string, bool) {
	mapped, ok := node.(map[string]any)
	if !ok {
		return "", false
	}

	ref, ok := mapped["$ref"].(string)

	return ref, ok
}

// diffCompatValue compares two schema nodes and reports every breaking change.
func diffCompatValue(schema, path string, oldNode, newNode any, oldSide, newSide string, resolver *compatResolver) []compatBreak {
	oldNode, newNode, refBreaks, resolved := resolveCompatRefs(schema, path, oldNode, newNode, oldSide, newSide, resolver)
	if !resolved {
		return refBreaks
	}

	oldMap, oldIsMap := oldNode.(map[string]any)
	newMap, newIsMap := newNode.(map[string]any)

	if !oldIsMap || !newIsMap {
		if reflect.DeepEqual(oldNode, newNode) {
			return nil
		}

		return []compatBreak{{Schema: schema, Path: path, Rule: "shape changed", Detail: renderCompatJSON(oldNode) + " → " + renderCompatJSON(newNode)}}
	}

	return slices.Concat(
		diffCompatType(schema, path, oldMap["type"], newMap["type"]),
		diffCompatObject(schema, path, oldMap, newMap, oldSide, newSide, resolver),
		diffCompatRequired(schema, path, oldMap, newMap),
		diffCompatConstraints(schema, path, oldMap, newMap),
		diffCompatComposed(schema, path, oldMap, newMap, oldSide, newSide, resolver),
		diffCompatUnknown(schema, path, oldMap, newMap),
	)
}

// compatTypeSet normalizes a type keyword to a set of type names.
func compatTypeSet(value any) map[string]bool {
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
	oldSet := compatTypeSet(oldValue)
	newSet := compatTypeSet(newValue)

	if reflect.DeepEqual(oldSet, newSet) {
		return nil
	}

	if len(oldSet) == 0 || len(newSet) == 0 {
		return []compatBreak{{Schema: schema, Path: path, Rule: "type changed", Detail: renderCompatJSON(oldValue) + " → " + renderCompatJSON(newValue)}}
	}

	oldNull := oldSet["null"]
	newNull := newSet["null"]

	if reflect.DeepEqual(compatBaseTypes(oldSet), compatBaseTypes(newSet)) {
		if oldNull && !newNull {
			return nil
		}

		return []compatBreak{{Schema: schema, Path: path, Rule: "now nullable", Detail: "old clients reject null"}}
	}

	return []compatBreak{{Schema: schema, Path: path, Rule: "type changed", Detail: renderCompatJSON(oldValue) + " → " + renderCompatJSON(newValue)}}
}

// compatBaseTypes returns the type set without null.
func compatBaseTypes(set map[string]bool) map[string]bool {
	base := make(map[string]bool, len(set))

	for name := range set {
		if name != "null" {
			base[name] = true
		}
	}

	return base
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

// diffCompatObject compares properties, required and items of two object schemas.
func diffCompatObject(schema, path string, oldMap, newMap map[string]any, oldSide, newSide string, resolver *compatResolver) []compatBreak {
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

	breaks := make([]compatBreak, 0, len(added)+len(removed)+len(oldProps))

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
		newChild, ok := newProps[name]
		if !ok {
			continue
		}

		breaks = append(breaks, diffCompatValue(schema, compatPath(path, name), oldChild, newChild, oldSide, newSide, resolver)...)
	}

	return append(breaks, diffCompatItems(schema, path, oldMap["items"], newMap["items"], oldSide, newSide, resolver)...)
}

// resolveCompatRefs follows $refs on either side. It returns the resolved nodes with
// resolved true, or a report with resolved false when there is nothing more to compare.
func resolveCompatRefs(schema, path string, oldNode, newNode any, oldSide, newSide string, resolver *compatResolver) (any, any, []compatBreak, bool) {
	oldRef, oldIsRef := compatRefOf(oldNode)
	newRef, newIsRef := compatRefOf(newNode)

	if !oldIsRef && !newIsRef {
		return oldNode, newNode, nil, true
	}

	if oldIsRef && newIsRef && oldRef == newRef {
		return resolveCompatSameRef(schema, path, oldSide, newSide, oldRef, resolver)
	}

	if oldIsRef {
		resolved, err := resolver.resolve(oldSide, oldRef)
		if err != nil {
			return nil, nil, []compatBreak{{Schema: schema, Path: path, Rule: "reference unresolvable", Detail: oldRef}}, false
		}

		oldNode = resolved
	}

	if newIsRef {
		resolved, err := resolver.resolve(newSide, newRef)
		if err != nil {
			return nil, nil, []compatBreak{{Schema: schema, Path: path, Rule: "reference unresolvable", Detail: newRef}}, false
		}

		newNode = resolved
	}

	return oldNode, newNode, nil, true
}

// resolveCompatSameRef compares two nodes that reference the same target.
func resolveCompatSameRef(schema, path, oldSide, newSide, ref string, resolver *compatResolver) (any, any, []compatBreak, bool) {
	resolvedOld, oldErr := resolver.resolve(oldSide, ref)
	resolvedNew, newErr := resolver.resolve(newSide, ref)

	if oldErr == nil && newErr == nil && reflect.DeepEqual(resolvedOld, resolvedNew) {
		return nil, nil, nil, false
	}

	if oldErr != nil {
		return nil, nil, []compatBreak{{Schema: schema, Path: path, Rule: "reference unresolvable", Detail: ref}}, false
	}

	if newErr != nil {
		return nil, nil, []compatBreak{{Schema: schema, Path: path, Rule: "reference unresolvable", Detail: ref}}, false
	}

	return resolvedOld, resolvedNew, nil, true
}

// compatPath joins a child name onto a dotted schema path.
func compatPath(path, name string) string {
	if path == "" {
		return name
	}

	return path + "." + name
}

// diffCompatItems compares array item schemas, including tuple form.
func diffCompatItems(schema, path string, oldNode, newNode any, oldSide, newSide string, resolver *compatResolver) []compatBreak {
	if oldNode == nil && newNode == nil {
		return nil
	}

	if oldNode == nil || newNode == nil {
		return []compatBreak{{Schema: schema, Path: path, Rule: "array items changed", Detail: renderCompatJSON(oldNode) + " → " + renderCompatJSON(newNode)}}
	}

	oldTuple, oldIsTuple := oldNode.([]any)
	newTuple, newIsTuple := newNode.([]any)

	if oldIsTuple || newIsTuple {
		if !oldIsTuple || !newIsTuple || len(oldTuple) != len(newTuple) {
			return []compatBreak{{Schema: schema, Path: path, Rule: "tuple shape changed", Detail: renderCompatJSON(oldNode) + " → " + renderCompatJSON(newNode)}}
		}

		var breaks []compatBreak

		for i := range oldTuple {
			breaks = append(breaks, diffCompatValue(schema, fmt.Sprintf("%s[%d]", path, i), oldTuple[i], newTuple[i], oldSide, newSide, resolver)...)
		}

		return breaks
	}

	return diffCompatValue(schema, path+"[]", oldNode, newNode, oldSide, newSide, resolver)
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
		if _, existed := oldProps[name]; !existed {
			continue
		}

		if !oldRequired[name] {
			breaks = append(breaks, compatBreak{Schema: schema, Path: compatPath(path, name), Rule: "now required", Detail: "old servers omit it"})
		}
	}

	for name := range oldRequired {
		if _, kept := newProps[name]; !kept {
			continue
		}

		if !newRequired[name] {
			breaks = append(breaks, compatBreak{Schema: schema, Path: compatPath(path, name), Rule: "now optional", Detail: "old clients still require it"})
		}
	}

	return breaks
}

// diffCompatConstraints compares value keywords. Any addition, removal or change breaks a laggard.
func diffCompatConstraints(schema, path string, nodeOld, nodeNew map[string]any) []compatBreak {
	var breaks []compatBreak

	for _, keyword := range compatConstraintKeywords {
		oldValue, oldOK := nodeOld[keyword]
		newValue, newOK := nodeNew[keyword]

		switch {
		case !oldOK && !newOK:
			continue
		case !oldOK || !newOK || !reflect.DeepEqual(oldValue, newValue):
			breaks = append(breaks, compatBreak{Schema: schema, Path: path, Rule: "constraint " + keyword + " changed", Detail: renderCompatJSON(oldValue) + " → " + renderCompatJSON(newValue)})
		}
	}

	return breaks
}

// diffCompatComposed compares anyOf, oneOf and allOf branches by index.
func diffCompatComposed(schema, path string, oldMap, newMap map[string]any, oldSide, newSide string, resolver *compatResolver) []compatBreak {
	var breaks []compatBreak

	for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
		oldBranches, oldOK := oldMap[keyword].([]any)
		newBranches, newOK := newMap[keyword].([]any)

		switch {
		case !oldOK && !newOK:
			continue
		case !oldOK || !newOK:
			breaks = append(breaks, compatBreak{Schema: schema, Path: path, Rule: "composition " + keyword + " changed", Detail: renderCompatJSON(oldMap[keyword]) + " → " + renderCompatJSON(newMap[keyword])})
		case len(oldBranches) != len(newBranches):
			breaks = append(breaks, compatBreak{Schema: schema, Path: path, Rule: "composition " + keyword + " changed", Detail: fmt.Sprintf("%d → %d branches", len(oldBranches), len(newBranches))})
		default:
			for i := range oldBranches {
				breaks = append(breaks, diffCompatValue(schema, fmt.Sprintf("%s[%d]", path, i), oldBranches[i], newBranches[i], oldSide, newSide, resolver)...)
			}
		}
	}

	return breaks
}

// diffCompatUnknown flags changes to validation keywords the classifier does not model, and ignores metadata.
func diffCompatUnknown(schema, path string, oldMap, newMap map[string]any) []compatBreak {
	known := map[string]bool{
		"$ref": true, "type": true, "properties": true, "required": true, "items": true,
		"anyOf": true, "oneOf": true, "allOf": true, "prefixItems": true, "contains": true,
	}

	for _, keyword := range compatConstraintKeywords {
		known[keyword] = true
	}

	for _, keyword := range compatMetadataKeywords {
		known[keyword] = true
	}

	names := map[string]bool{}

	for keyword := range oldMap {
		names[keyword] = true
	}

	for keyword := range newMap {
		names[keyword] = true
	}

	var breaks []compatBreak

	for keyword := range names {
		if known[keyword] || strings.HasPrefix(keyword, "x-") {
			continue
		}

		if !reflect.DeepEqual(oldMap[keyword], newMap[keyword]) {
			breaks = append(breaks, compatBreak{Schema: schema, Path: path, Rule: "keyword " + keyword + " changed", Detail: renderCompatJSON(oldMap[keyword]) + " → " + renderCompatJSON(newMap[keyword])})
		}
	}

	return breaks
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

// loadCompatBaseline reads one ref's schema documents from git.
func loadCompatBaseline(dir, ref string) (map[string]map[string]any, error) {
	schemasDir := filepath.Join("contract", "schemas") + "/"

	listCmd := exec.CommandContext(context.Background(), "git", "-C", dir, "ls-tree", "--name-only", ref, "--", schemasDir)

	listOut, err := listCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read %s from git: %w", ref, err)
	}

	docs := map[string]map[string]any{}

	for line := range strings.SplitSeq(strings.TrimSpace(string(listOut)), "\n") {
		if line == "" {
			continue
		}

		showCmd := exec.CommandContext(context.Background(), "git", "-C", dir, "show", ref+":"+line)

		data, err := showCmd.Output()
		if err != nil {
			return nil, fmt.Errorf("read %s from git: %w", line, err)
		}

		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", line, err)
		}

		docs[filepath.Base(line)] = doc
	}

	return docs, nil
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

	baseline, err := loadCompatBaseline(abs, ref)
	if err != nil {
		return err
	}

	breaks := compatDocs(baseline, current)

	slices.SortFunc(breaks, func(a, b compatBreak) int {
		if a.Schema != b.Schema {
			return strings.Compare(a.Schema, b.Schema)
		}

		return strings.Compare(a.Path, b.Path)
	})

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
