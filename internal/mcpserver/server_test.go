package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/mcp"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/render"
	"github.com/taituo/simo/internal/spec"
	"github.com/taituo/simo/internal/store"
)

var (
	worldsOnce sync.Once
	worldsDir  string
	worldsErr  error
)

// worlds builds two worlds once: retail (2 days, both stored) and tcp.
func worlds(t *testing.T) string {
	t.Helper()
	worldsOnce.Do(func() {
		worldsDir, worldsErr = os.MkdirTemp("", "simo-mcp-test-")
		if worldsErr != nil {
			return
		}
		for name, opt := range map[string]store.BuildOptions{
			"retail": {To: 48 * 3600, Materialize: true},
			"tcp":    {},
		} {
			file := map[string]string{"retail": "retail-pos.yaml", "tcp": "tcp-connections.yaml"}[name]
			raw, err := os.ReadFile(filepath.Join("..", "..", "examples", file))
			if err != nil {
				worldsErr = err
				return
			}
			s, err := spec.Parse(raw)
			if err != nil {
				worldsErr = err
				return
			}
			w, err := compile.Compile(s, raw)
			if err != nil {
				worldsErr = err
				return
			}
			if _, worldsErr = store.Build(filepath.Join(worldsDir, name), w, raw, opt); worldsErr != nil {
				return
			}
		}
	})
	if worldsErr != nil {
		t.Fatal(worldsErr)
	}
	return worldsDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if worldsDir != "" {
		os.RemoveAll(worldsDir)
	}
	os.Exit(code)
}

type client struct {
	t  *testing.T
	s  *mcp.Server
	id int
}

func newClient(t *testing.T, cfg Config) *client {
	t.Helper()
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	s, closeFn, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFn() })
	return &client{t: t, s: s}
}

func (c *client) rpc(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	var out map[string]any
	if err := json.Unmarshal(c.s.HandleMessage(context.Background(), msg), &out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

// call invokes a tool and returns its text and whether it is an error.
func (c *client) call(tool string, args map[string]any) (string, bool) {
	c.t.Helper()
	r := c.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	if e, ok := r["error"]; ok {
		return e.(map[string]any)["message"].(string), true
	}
	res := r["result"].(map[string]any)
	return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
}

func (c *client) ok(tool string, args map[string]any) string {
	c.t.Helper()
	text, isErr := c.call(tool, args)
	if isErr {
		c.t.Fatalf("%s(%v) failed: %s", tool, args, text)
	}
	return text
}

func (c *client) tools() []string {
	c.t.Helper()
	var names []string
	for _, x := range c.rpc("tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	return names
}

func TestRoles(t *testing.T) {
	dir := worlds(t)
	want := map[string][]string{
		RoleObserver: {"describe_world", "get_metrics", "list_changes", "list_devices", "list_worlds", "search_logs", "summarize_logs"},
		RoleAuthor:   {"create_world", "get_example", "get_seed_schema", "list_examples", "list_worlds", "preview_seed", "validate_seed"},
		RoleAdmin:    {"get_truth"},
	}
	for role, tools := range want {
		c := newClient(t, Config{WorldsDir: dir, Roles: []string{role}})
		if got := c.tools(); !slices.Equal(got, tools) {
			t.Errorf("role %s: tools %v, want %v", role, got, tools)
		}
	}
	obs := newClient(t, Config{WorldsDir: dir, Roles: []string{RoleObserver}})
	if text, isErr := obs.call("get_truth", map[string]any{"world": "retail"}); !isErr || !strings.Contains(text, "unknown tool") {
		t.Errorf("observer could call get_truth: %s", text)
	}
	if _, _, err := New(Config{Roles: []string{"root"}}); err == nil {
		t.Error("unknown role accepted")
	}
}

func TestDescribeRevealsNoHiddenState(t *testing.T) {
	c := newClient(t, Config{WorldsDir: worlds(t), Roles: []string{RoleObserver}})
	text := c.ok("describe_world", map[string]any{"world": "retail"})
	for _, want := range []string{"pos_terminal: 123 across 20 sites", "pos_terminal/reader_timeout WARN", "pos_terminal/temp_c C", "stored days: 2 of 7"} {
		if !strings.Contains(text, want) {
			t.Errorf("describe_world lacks %q:\n%s", want, text)
		}
	}
	for _, hidden := range []string{"degraded", "health", "rebooting", "hardware", "firmware bug"} {
		if strings.Contains(text, hidden) {
			t.Errorf("describe_world reveals hidden %q", hidden)
		}
	}
	if text, isErr := c.call("describe_world", nil); !isErr || !strings.Contains(text, "retail, tcp") {
		t.Errorf("ambiguous world: %s", text)
	}
}

func TestSearchPagination(t *testing.T) {
	dir := worlds(t)
	c := newClient(t, Config{WorldsDir: dir, Roles: []string{RoleObserver}})
	// RFC 3339 times, as an agent would copy them from log lines.
	args := map[string]any{"world": "retail", "from": "2026-10-06T09:00:00Z", "to": "2026-10-06T10:00:00Z", "device": "pos-07-*", "limit": 37}
	var paged []string
	pages := 0
	for {
		text := c.ok("search_logs", args)
		pages++
		lines := strings.Split(strings.TrimSpace(text), "\n")
		last := lines[len(lines)-1]
		body := lines[1 : len(lines)-1]
		paged = append(paged, body...)
		cursor, more := strings.CutPrefix(last, "next_cursor: ")
		if !more {
			if last != "end of results" {
				t.Fatalf("unexpected footer %q", last)
			}
			break
		}
		if len(body) != 37 {
			t.Fatalf("page %d has %d lines, want 37", pages, len(body))
		}
		args = map[string]any{"cursor": cursor, "limit": 37}
	}

	// The same window straight from the store.
	st, err := store.Open(filepath.Join(dir, "retail"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rend, _ := render.New(st.World)
	var ids []uint32
	for _, d := range st.World.Devices {
		if strings.HasPrefix(d.Name, "pos-07-") {
			ids = append(ids, d.ID)
		}
	}
	var want []string
	st.Events(store.Query{FromNs: 33 * int64(time.Hour), ToNs: 34 * int64(time.Hour), Devices: ids}, func(r *record.Record) error {
		want = append(want, string(rend.Line(nil, r, render.Text)))
		return nil
	})
	if pages < 3 || !slices.Equal(paged, want) {
		t.Errorf("paged %d lines over %d pages, store has %d; equal=%v", len(paged), pages, len(want), slices.Equal(paged, want))
	}
}

func TestSearchFiltersAndLimits(t *testing.T) {
	c := newClient(t, Config{WorldsDir: worlds(t), Roles: []string{RoleObserver}})
	text := c.ok("search_logs", map[string]any{"world": "retail", "from": "day2 09:00", "for": "1h", "level": "WARN", "limit": 500})
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if lines[0] != "200 lines" {
		t.Errorf("limit not capped at 200: %q", lines[0])
	}
	for _, l := range lines[1 : len(lines)-1] {
		if !strings.Contains(l, " WARN ") && !strings.Contains(l, " ERROR ") {
			t.Fatalf("line below WARN: %s", l)
		}
	}
	text = c.ok("search_logs", map[string]any{"world": "retail", "from": "day2 09:15", "for": "40m", "contains": "card reader", "device": "pos-07-03"})
	if !strings.Contains(text, "pos-07-03 WARN card reader timeout") {
		t.Errorf("contains filter: %s", text)
	}
	if text, isErr := c.call("search_logs", map[string]any{"world": "retail", "event": "nope"}); !isErr || !strings.Contains(text, "describe_world") {
		t.Errorf("unknown event: %s", text)
	}
	if text, isErr := c.call("search_logs", map[string]any{"world": "retail", "levle": "WARN"}); !isErr || !strings.Contains(text, "levle") {
		t.Errorf("misspelled argument not reported: %s", text)
	}
}

func TestSummarizeMetricsChanges(t *testing.T) {
	c := newClient(t, Config{WorldsDir: worlds(t), Roles: []string{RoleObserver}})
	text := c.ok("summarize_logs", map[string]any{"world": "retail", "from": "day2 08:00", "to": "day2 11:00", "by": []string{"level"}, "step": "1h", "device": "pos-07-*"})
	if !regexp.MustCompile(`2026-10-06 09:00\s+WARN\s+113`).MatchString(text) {
		t.Errorf("summarize_logs does not show the fault hour:\n%s", text)
	}
	text = c.ok("get_metrics", map[string]any{"world": "retail", "device": "pos-07-02", "from": "day2 09:00", "for": "1h", "step": "15m"})
	if strings.Count(text, "temp_c") != 4 {
		t.Errorf("get_metrics:\n%s", text)
	}
	if text, isErr := c.call("get_metrics", map[string]any{"world": "retail", "device": "pos-07-02", "step": "1m"}); !isErr || !strings.Contains(text, "too many") {
		t.Errorf("uncapped metrics: %s", text)
	}
	text = c.ok("list_changes", map[string]any{"world": "retail"})
	if !strings.Contains(text, "on-call operator ran restart") {
		t.Errorf("list_changes:\n%s", text)
	}
	text = c.ok("list_devices", map[string]any{"world": "retail", "limit": 50})
	if !strings.HasPrefix(text, "50 of 123 matching devices") || !strings.Contains(text, "next_cursor: devices:50") {
		t.Errorf("list_devices:\n%s", text)
	}
}

func TestPinnedWorld(t *testing.T) {
	c := newClient(t, Config{WorldsDir: worlds(t), World: "retail", Roles: []string{RoleObserver}})
	if text := c.ok("list_worlds", nil); strings.Contains(text, "tcp") {
		t.Errorf("pinned server lists other worlds: %s", text)
	}
	if text, isErr := c.call("describe_world", map[string]any{"world": "tcp"}); !isErr || !strings.Contains(text, "only world") {
		t.Errorf("pinned server read another world: %s", text)
	}
	c.ok("describe_world", nil) // the pinned world is the default
	if text, isErr := c.call("describe_world", map[string]any{"world": "../retail"}); !isErr {
		t.Errorf("path in world name accepted: %s", text)
	}
}

const brokenSeed = `
simo: 1
name: kv-store
master_seed: 5
clock: { start: 2026-10-05T00:00:00Z, duration: 1d, tick: 1s }
classes:
  kv:
    states:
      serving:   { to: { compacting: 1/6h, crashed: 1/2fortnight } }
      compacting: { to: { serving: fixed(5m) } }
      crashed:   { emit: [crash] }
    events:
      - { id: get, level: INFO, text: "GET {key:hex(8)} {ms:lognormal(1, 0.5)|%.1f}ms", rate: 0.5/s, only_in: [serving, compacting] }
      - { id: slow, level: WARN, text: "slow GET {ms:uniform(200, 900)|%d}ms", rate: 0.001/s, in_state: { compactin: x50 } }
      - { id: crash, level: ERROR, text: "process exited with signal {sig:choice(SIGSEGV, SIGKILL)}" }
fleet:
  - { class: kv, count: 12 }
`

// The author loop an LLM follows, scripted: validate, fix, preview, create,
// then read the new world as an observer.
func TestAuthorLoop(t *testing.T) {
	dir := t.TempDir()
	c := newClient(t, Config{WorldsDir: dir, Roles: []string{RoleAuthor, RoleObserver}})
	if schema := c.ok("get_seed_schema", nil); !json.Valid([]byte(schema)) {
		t.Fatal("schema is not JSON")
	}
	if ex := c.ok("list_examples", nil); !strings.Contains(ex, "retail-pos: A retail chain") {
		t.Errorf("list_examples: %s", ex)
	}

	text := c.ok("validate_seed", map[string]any{"seed": brokenSeed})
	for _, want := range []string{
		"invalid: 2 errors",
		"classes.kv.states.serving.to.crashed: bad rate \"1/2fortnight\"", // unknown unit
		"classes.kv.events[1].in_state.compactin: unknown state",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("validate_seed lacks %q:\n%s", want, text)
		}
	}
	fixed := strings.NewReplacer("1/2fortnight", "1/12h", "compactin:", "compacting:").Replace(brokenSeed)
	text = c.ok("validate_seed", map[string]any{"seed": fixed})
	if !strings.HasPrefix(text, "valid: kv-store, 1 class, 12 devices") || !strings.Contains(text, "only a command can leave") && !strings.Contains(text, "absorbing state") {
		t.Errorf("validate_seed on the fixed seed:\n%s", text)
	}
	text = c.ok("preview_seed", map[string]any{"seed": fixed, "for": "2h", "lines": 5})
	if !strings.Contains(text, "kv/get") || !strings.Contains(text, "sample lines:") {
		t.Errorf("preview_seed:\n%s", text)
	}
	text = c.ok("create_world", map[string]any{"name": "kv", "seed": fixed, "materialize": true})
	if !strings.Contains(text, "created world kv") || !strings.Contains(text, "1 day stored") {
		t.Errorf("create_world:\n%s", text)
	}
	if text, isErr := c.call("create_world", map[string]any{"name": "kv", "seed": fixed}); !isErr || !strings.Contains(text, "already exists") {
		t.Errorf("duplicate world: %s", text)
	}
	text = c.ok("summarize_logs", map[string]any{"world": "kv", "by": []string{"event"}})
	if !strings.Contains(text, "kv/get") {
		t.Errorf("observer view of the new world:\n%s", text)
	}

	// Expensive previews are refused.
	huge := strings.Replace(fixed, "count: 12", "count: 100000", 1)
	if text, isErr := c.call("preview_seed", map[string]any{"seed": huge, "for": "1d"}); !isErr || !strings.Contains(text, "limit") {
		t.Errorf("expensive preview: %s", text)
	}
	if text, isErr := c.call("get_example", map[string]any{"name": "../go"}); !isErr {
		t.Errorf("path in example name accepted: %s", text)
	}
}

func TestResources(t *testing.T) {
	c := newClient(t, Config{WorldsDir: worlds(t), Roles: []string{RoleAuthor, RoleObserver}})
	read := func(uri string) string {
		r := c.rpc("resources/read", map[string]any{"uri": uri})
		if e, ok := r["error"]; ok {
			t.Fatalf("%s: %v", uri, e)
		}
		return r["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"].(string)
	}
	if !json.Valid([]byte(read("simo://schema/seed"))) {
		t.Error("schema resource is not JSON")
	}
	if !strings.Contains(read("simo://examples/tcp-connections"), "RFC 9293") {
		t.Error("example resource")
	}
	if !strings.Contains(read("simo://worlds/tcp/summary"), "world tcp-connections") {
		t.Error("world summary resource")
	}
	list := c.rpc("resources/list", nil)["result"].(map[string]any)["resources"].([]any)
	var uris []string
	for _, r := range list {
		uris = append(uris, r.(map[string]any)["uri"].(string))
	}
	for _, want := range []string{"simo://schema/seed", "simo://examples/retail-pos", "simo://worlds/retail/summary", "simo://worlds/tcp/summary"} {
		if !slices.Contains(uris, want) {
			t.Errorf("resources/list lacks %s: %v", want, uris)
		}
	}
}
