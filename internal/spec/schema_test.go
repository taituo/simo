package spec

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(SchemaJSON, &s); err != nil {
		t.Fatalf("schema.json is not valid JSON: %v", err)
	}
	return s
}

// schemaAt walks a path of keys into the schema.
func schemaAt(t *testing.T, s map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := s
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("schema has no %s", strings.Join(path, "."))
		}
		cur = next
	}
	return cur
}

// The schema and the Go types must list the same fields, so the schema an
// LLM reads cannot drift from what the parser accepts.
func TestSchemaMatchesGoTypes(t *testing.T) {
	s := loadSchema(t)
	cases := []struct {
		typ  any
		path []string
	}{
		{Seed{}, nil},
		{Clock{}, []string{"$defs", "clock"}},
		{Pattern{}, []string{"$defs", "pattern"}},
		{Fourier{}, []string{"$defs", "fourier"}},
		{Vocab{}, []string{"$defs", "vocab"}},
		{Class{}, []string{"$defs", "class"}},
		{Health{}, []string{"$defs", "health"}},
		{State{}, []string{"$defs", "state"}},
		{Metric{}, []string{"$defs", "metric"}},
		{OU{}, []string{"$defs", "metric", "properties", "ou"}},
		{MetricShift{}, []string{"$defs", "metric_shift"}},
		{Event{}, []string{"$defs", "event"}},
		{Command{}, []string{"$defs", "command"}},
		{FleetEntry{}, []string{"$defs", "fleet_entry"}},
		{Fault{}, []string{"$defs", "fault"}},
		{Effect{}, []string{"$defs", "effect"}},
		{Operators{}, []string{"$defs", "operators"}},
		{Responder{}, []string{"$defs", "operators", "properties", "responder"}},
	}
	for _, c := range cases {
		rt := reflect.TypeOf(c.typ)
		var goFields []string
		for i := 0; i < rt.NumField(); i++ {
			if tag := strings.Split(rt.Field(i).Tag.Get("yaml"), ",")[0]; tag != "" && tag != "-" {
				goFields = append(goFields, tag)
			}
		}
		props := schemaAt(t, s, append(c.path, "properties")...)
		var schemaFields []string
		for k := range props {
			schemaFields = append(schemaFields, k)
		}
		sort.Strings(goFields)
		sort.Strings(schemaFields)
		if !slices.Equal(goFields, schemaFields) {
			t.Errorf("%s: Go fields %v, schema properties %v", rt.Name(), goFields, schemaFields)
		}
		if ap, ok := schemaAt(t, s, c.path...)["additionalProperties"]; !ok || ap != false {
			t.Errorf("%s: schema should set additionalProperties: false so typos are caught", rt.Name())
		}
	}
}

func yamlToJSONValue(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExamplesConformToSchema(t *testing.T) {
	s := loadSchema(t)
	paths, _ := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if len(paths) == 0 {
		t.Fatal("no examples")
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range (&schemaChecker{root: s}).check(s, yamlToJSONValue(t, raw), "") {
			t.Errorf("%s: %s", filepath.Base(p), e)
		}
	}
}

func TestSchemaRejectsBadSeeds(t *testing.T) {
	s := loadSchema(t)
	cases := map[string]string{
		"unknown key":    strings.Replace(base, "fleet:", "fleeet:", 1),
		"bad tick":       strings.Replace(base, "tick: 1s", "tick: soon", 1),
		"bad level":      strings.Replace(base, "level: INFO", "level: LOUD", 1),
		"bad transition": strings.Replace(base, "bad: 1/1h", "bad: whenever", 1),
	}
	for name, src := range cases {
		if errs := (&schemaChecker{root: s}).check(s, yamlToJSONValue(t, []byte(src)), ""); len(errs) == 0 {
			t.Errorf("%s: schema accepted it", name)
		}
	}
}

// schemaChecker validates JSON values against the subset of JSON Schema the
// seed schema uses. It exists only to test the schema.
type schemaChecker struct{ root map[string]any }

func (c *schemaChecker) check(schema any, v any, path string) []string {
	sch, ok := schema.(map[string]any)
	if !ok {
		if b, isBool := schema.(bool); isBool && !b {
			return []string{path + ": not allowed"}
		}
		return nil
	}
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, path+": "+fmt.Sprintf(format, a...)) }
	if ref, ok := sch["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		def := c.root["$defs"].(map[string]any)[name]
		if def == nil {
			return []string{path + ": unresolved " + ref}
		}
		errs = append(errs, c.check(def, v, path)...)
	}
	if want, ok := sch["const"]; ok && !reflect.DeepEqual(want, v) {
		fail("want %v", want)
	}
	if enum, ok := sch["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return reflect.DeepEqual(e, v) }) {
		fail("%v is not one of %v", v, enum)
	}
	if typ, ok := sch["type"]; ok {
		var types []string
		switch tt := typ.(type) {
		case string:
			types = []string{tt}
		case []any:
			for _, x := range tt {
				types = append(types, x.(string))
			}
		}
		if !slices.ContainsFunc(types, func(t string) bool { return hasType(v, t) }) {
			fail("want type %v, got %T", types, v)
			return errs
		}
	}
	if str, ok := v.(string); ok {
		if pat, ok := sch["pattern"].(string); ok && !regexp.MustCompile(pat).MatchString(str) {
			fail("%q does not match %s", str, pat)
		}
	}
	if num, ok := v.(float64); ok {
		if m, ok := sch["minimum"].(float64); ok && num < m {
			fail("%v is below %v", num, m)
		}
		if m, ok := sch["exclusiveMinimum"].(float64); ok && num <= m {
			fail("%v is not above %v", num, m)
		}
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := sch["properties"].(map[string]any)
		for _, r := range asStrings(sch["required"]) {
			if _, ok := obj[r]; !ok {
				fail("missing %q", r)
			}
		}
		if n, ok := sch["minProperties"].(float64); ok && float64(len(obj)) < n {
			fail("needs at least %v properties", n)
		}
		if n, ok := sch["maxProperties"].(float64); ok && float64(len(obj)) > n {
			fail("allows at most %v properties", n)
		}
		for k, val := range obj {
			if pn, ok := sch["propertyNames"]; ok {
				errs = append(errs, c.check(pn, k, path+"."+k+" (name)")...)
			}
			if ps, ok := props[k]; ok {
				errs = append(errs, c.check(ps, val, path+"."+k)...)
			} else if ap, ok := sch["additionalProperties"]; ok {
				errs = append(errs, c.check(ap, val, path+"."+k)...)
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if n, ok := sch["minItems"].(float64); ok && float64(len(arr)) < n {
			fail("needs at least %v items", n)
		}
		if n, ok := sch["maxItems"].(float64); ok && float64(len(arr)) > n {
			fail("allows at most %v items", n)
		}
		prefix, _ := sch["prefixItems"].([]any)
		for i, item := range arr {
			p := fmt.Sprintf("%s[%d]", path, i)
			if i < len(prefix) {
				errs = append(errs, c.check(prefix[i], item, p)...)
			} else if is, ok := sch["items"]; ok {
				errs = append(errs, c.check(is, item, p)...)
			}
		}
	}
	return errs
}

func hasType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "null":
		return v == nil
	}
	return false
}

func asStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, x.(string))
	}
	return out
}
