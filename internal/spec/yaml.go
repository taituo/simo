package spec

import (
	"fmt"
	"os"
	"time"

	"github.com/goccy/go-yaml"
)

// Expr is a scalar in the seed DSL, such as "1/2h", "x40" or "uniform(4, 8)".
// It accepts YAML strings, numbers and timestamps.
type Expr string

// UnmarshalYAML implements yaml.BytesUnmarshaler.
func (e *Expr) UnmarshalYAML(b []byte) error {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		*e = ""
	case string:
		*e = Expr(x)
	case time.Time:
		*e = Expr(x.UTC().Format(time.RFC3339Nano))
	case []any, map[string]any, yaml.MapSlice:
		return fmt.Errorf("expected a single value, got a list or map")
	default:
		*e = Expr(fmt.Sprint(x))
	}
	return nil
}

func (e Expr) String() string { return string(e) }

// OrderedMap is a YAML mapping that keeps its key order, so declaration order
// (for example of states) is meaningful and deterministic.
type OrderedMap[V any] struct {
	Keys   []string
	Values map[string]V
}

// UnmarshalYAML implements yaml.BytesUnmarshaler.
func (m *OrderedMap[V]) UnmarshalYAML(b []byte) error {
	var ms yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(b, &ms, yaml.UseOrderedMap()); err != nil {
		return err
	}
	m.Keys = nil
	m.Values = make(map[string]V, len(ms))
	for _, it := range ms {
		k := fmt.Sprint(it.Key)
		if _, dup := m.Values[k]; dup {
			return fmt.Errorf("duplicate key %q", k)
		}
		raw, err := yaml.Marshal(it.Value)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		var v V
		if err := yaml.UnmarshalWithOptions(raw, &v, yaml.Strict()); err != nil {
			return fmt.Errorf("%s: %s", k, yaml.FormatError(err, false, false))
		}
		m.Keys = append(m.Keys, k)
		m.Values[k] = v
	}
	return nil
}

// Len returns the number of keys.
func (m OrderedMap[V]) Len() int { return len(m.Keys) }

// Get returns the value for k.
func (m OrderedMap[V]) Get(k string) (V, bool) {
	v, ok := m.Values[k]
	return v, ok
}

// Index returns the position of k, or -1.
func (m OrderedMap[V]) Index(k string) int {
	for i, key := range m.Keys {
		if key == k {
			return i
		}
	}
	return -1
}

// Parse decodes a seed. Unknown fields are errors, so typos surface early.
func Parse(data []byte) (*Seed, error) {
	var s Seed
	if err := yaml.UnmarshalWithOptions(data, &s, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parse seed: %s", yaml.FormatError(err, false, true))
	}
	return &s, nil
}

// Load reads and decodes a seed file. It also returns the raw bytes, which
// are hashed into the world's manifest.
func Load(path string) (*Seed, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return s, data, nil
}
