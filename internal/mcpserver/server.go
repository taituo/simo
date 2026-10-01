// Package mcpserver exposes simo over MCP with three roles:
//
//   - observer: read a world the way an on-call engineer would (devices,
//     counts, log search, metrics, operator actions), never hidden state
//   - author: read the seed schema and examples, validate and preview seeds,
//     and build worlds
//   - admin: the answer key (ground truth)
//
// An agent under test gets only the observer role.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/taituo/simo/examples"
	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/mcp"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/render"
	"github.com/taituo/simo/internal/report"
	"github.com/taituo/simo/internal/spec"
	"github.com/taituo/simo/internal/store"
)

// Roles.
const (
	RoleObserver = "observer"
	RoleAuthor   = "author"
	RoleAdmin    = "admin"
)

// Config configures the server.
type Config struct {
	WorldsDir string   // directory of worlds, one subdirectory each
	World     string   // if set, the only world observer tools may read
	Roles     []string // observer, author, admin
	Version   string
	// MaxDeviceTicks caps devices x ticks simulated by preview_seed and
	// create_world; 0 means 2e9 (a week of 3,000 devices at 1 s ticks).
	MaxDeviceTicks int64
	// Workers for the engine; 0 means all CPUs.
	Workers int
}

// Limits on tool output, to keep agent context small.
const (
	maxSearchLines  = 200
	defSearchLines  = 50
	maxRollupRows   = 300
	maxMetricRows   = 500
	maxDeviceRows   = 1000
	defDeviceRows   = 100
	maxChangeRows   = 500
	defMaxDevTicks  = 2e9
	defPreviewLines = 20
)

var worldNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type server struct {
	cfg    Config
	mu     sync.Mutex // serializes tool calls; stores are not goroutine-safe
	stores map[string]*store.Store
}

// New builds the MCP server. Call the returned close function when done.
func New(cfg Config) (*mcp.Server, func() error, error) {
	if cfg.MaxDeviceTicks <= 0 {
		cfg.MaxDeviceTicks = defMaxDevTicks
	}
	roles := map[string]bool{}
	for _, r := range cfg.Roles {
		switch r {
		case RoleObserver, RoleAuthor, RoleAdmin:
			roles[r] = true
		default:
			return nil, nil, fmt.Errorf("unknown role %q (want observer, author or admin)", r)
		}
	}
	if len(roles) == 0 {
		return nil, nil, errors.New("give at least one role")
	}
	if cfg.World != "" && !worldNameRe.MatchString(cfg.World) {
		return nil, nil, fmt.Errorf("bad world name %q", cfg.World)
	}
	s := &server{cfg: cfg, stores: map[string]*store.Store{}}
	m := &mcp.Server{Name: "simo", Version: cfg.Version}
	var guide []string
	if roles[RoleObserver] {
		s.registerObserver(m)
		guide = append(guide, "To investigate a world: describe_world first, then summarize_logs to see where and when things happen (counts by event, level, device or site), then search_logs in narrow windows, get_metrics for one device, and list_changes for operator actions. Times accept RFC 3339 timestamps copied from log lines, \"day2 09:15\" (day 1 is the first), \"09:15\", or offsets like 26h. Results are capped; follow next_cursor to page.")
	}
	if roles[RoleAuthor] {
		s.registerAuthor(m)
		guide = append(guide, "To author a world: read get_seed_schema (or resource simo://schema/seed) and an example, write a seed in YAML, call validate_seed until it reports valid, check preview_seed that counts and sample lines look right, then create_world.")
	}
	if roles[RoleAdmin] {
		s.registerAdmin(m)
		guide = append(guide, "get_truth returns the answer key: scheduled faults with their devices and times.")
	}
	if roles[RoleObserver] || roles[RoleAuthor] {
		m.AddTool(mcp.Tool{
			Name: "list_worlds", Title: "List worlds", ReadOnly: true,
			Description: "List the worlds this server can read, with their simulated time range and size.",
			InputSchema: object(nil),
			Handler:     s.locked(s.listWorlds),
		})
	}
	m.Instructions = strings.Join(guide, "\n\n")
	return m, s.close, nil
}

func (s *server) locked(h func(context.Context, json.RawMessage) (string, error)) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return h(ctx, args)
	}
}

func (s *server) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for _, st := range s.stores {
		if err := st.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.stores = map[string]*store.Store{}
	return first
}

// --- JSON Schema helpers for tool inputs -------------------------------

func object(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "minimum": 0, "description": desc}
}

var (
	propWorld = str("World name; optional when the server has one world")
	propFrom  = str(`Window start: RFC 3339 (e.g. 2026-10-06T09:15:00Z), "day2 09:15", "09:15" or an offset like 26h. Default: start of the run`)
	propTo    = str("Window end, same forms as from. Default: end of the run, or from + for")
	propFor   = str("Window length instead of to, e.g. 30m or 2h")
)

func withWindow(props map[string]any) map[string]any {
	props["world"] = propWorld
	props["from"] = propFrom
	props["to"] = propTo
	props["for"] = propFor
	return props
}

// --- worlds ------------------------------------------------------------

func (s *server) worldNames() []string {
	entries, err := os.ReadDir(s.cfg.WorldsDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && worldNameRe.MatchString(e.Name()) {
			if _, err := os.Stat(filepath.Join(s.cfg.WorldsDir, e.Name(), store.WorldFile)); err == nil {
				names = append(names, e.Name())
			}
		}
	}
	sort.Strings(names)
	return names
}

func (s *server) world(name string) (*store.Store, error) {
	if s.cfg.World != "" {
		if name != "" && name != s.cfg.World {
			return nil, fmt.Errorf("this server serves only world %q", s.cfg.World)
		}
		name = s.cfg.World
	}
	if name == "" {
		names := s.worldNames()
		switch len(names) {
		case 0:
			return nil, errors.New("there are no worlds yet")
		case 1:
			name = names[0]
		default:
			return nil, fmt.Errorf("pass world, one of: %s", strings.Join(names, ", "))
		}
	}
	if !worldNameRe.MatchString(name) {
		return nil, fmt.Errorf("bad world name %q", name)
	}
	if st, ok := s.stores[name]; ok {
		return st, nil
	}
	dir := filepath.Join(s.cfg.WorldsDir, name)
	if _, err := os.Stat(filepath.Join(dir, store.WorldFile)); err != nil {
		return nil, fmt.Errorf("no world %q", name)
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	s.stores[name] = st
	return st, nil
}

func (s *server) listWorlds(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := mcp.Args(raw, &struct{}{}); err != nil {
		return "", err
	}
	names := s.worldNames()
	if s.cfg.World != "" {
		names = []string{s.cfg.World}
	}
	if len(names) == 0 {
		return "no worlds yet", nil
	}
	var b strings.Builder
	for _, n := range names {
		st, err := s.world(n)
		if err != nil {
			fmt.Fprintf(&b, "%s: %v\n", n, err)
			continue
		}
		w := st.World
		end := time.Duration(w.Ticks) * w.Tick
		fmt.Fprintf(&b, "%s: %s, %s to %s (%s)\n", n, report.Plural(len(w.Devices), "device"),
			w.Start.Format(time.RFC3339), w.Start.Add(end).Format(time.RFC3339), report.Span(end))
	}
	return b.String(), nil
}

// --- observer tools ----------------------------------------------------

func (s *server) registerObserver(m *mcp.Server) {
	m.AddTool(mcp.Tool{
		Name: "describe_world", Title: "Describe a world", ReadOnly: true,
		Description: "What can be observed in a world: its time range, devices per class, the log events that exist (with message templates) and metrics. Start here.",
		InputSchema: object(map[string]any{"world": propWorld}),
		Handler:     s.locked(s.describeWorld),
	})
	m.AddTool(mcp.Tool{
		Name: "list_devices", Title: "List devices", ReadOnly: true,
		Description: "List devices with class and site. Filter by class, site or name (a trailing * matches a prefix).",
		InputSchema: object(map[string]any{
			"world": propWorld, "class": str("Only this class"), "site": integer("Only this site"),
			"name": str("Device name, or a prefix ending in *"), "limit": integer(fmt.Sprintf("Rows to return, default %d, max %d", defDeviceRows, maxDeviceRows)),
			"cursor": str("next_cursor from a previous call"),
		}),
		Handler: s.locked(s.listDevices),
	})
	m.AddTool(mcp.Tool{
		Name: "summarize_logs", Title: "Summarize logs", ReadOnly: true,
		Description: "Event counts in a window, grouped by any of event, level, device, site, class, optionally per time step. Cheap; use it to find where and when to look before searching.",
		InputSchema: object(withWindow(map[string]any{
			"by":     map[string]any{"type": "array", "items": map[string]any{"enum": store.RollupKeys}, "description": "Group keys, default [event]"},
			"step":   str("Bucket size such as 5m or 1h; default one bucket for the whole window"),
			"device": str("Only this device, or a prefix ending in *"),
		})),
		Handler: s.locked(s.summarizeLogs),
	})
	m.AddTool(mcp.Tool{
		Name: "search_logs", Title: "Search logs", ReadOnly: true,
		Description: fmt.Sprintf("Log lines in time order, filtered by device, minimum level, event id and text. Returns at most %d lines per call (default %d) and a next_cursor to continue.", maxSearchLines, defSearchLines),
		InputSchema: object(withWindow(map[string]any{
			"device":   str("Device name, or a prefix ending in *"),
			"level":    map[string]any{"enum": spec.Levels, "description": "Minimum level"},
			"event":    str("Event id, e.g. reader_timeout"),
			"contains": str("Only lines whose message contains this text"),
			"limit":    integer(fmt.Sprintf("Lines to return, default %d, max %d", defSearchLines, maxSearchLines)),
			"cursor":   str("next_cursor from a previous call; other filters are then taken from the cursor"),
		})),
		Handler: s.locked(s.searchLogs),
	})
	m.AddTool(mcp.Tool{
		Name: "get_metrics", Title: "Get metrics", ReadOnly: true,
		Description: "A device's metric series: min, avg and max per time step.",
		InputSchema: object(withWindow(map[string]any{
			"device": str("Device name"),
			"metric": str("Metric name; default all of the device's metrics"),
			"step":   str("Bucket size, default 5m, at least 1m"),
		}), "device"),
		Handler: s.locked(s.getMetrics),
	})
	m.AddTool(mcp.Tool{
		Name: "list_changes", Title: "List operational changes", ReadOnly: true,
		Description: "Operator and manual actions in a window, such as restarts, newest last: the change log an on-call engineer would check.",
		InputSchema: object(withWindow(map[string]any{
			"device": str("Device name, or a prefix ending in *"),
			"limit":  integer(fmt.Sprintf("Rows to return, default 100, max %d", maxChangeRows)),
		})),
		Handler: s.locked(s.listChanges),
	})
	m.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "simo://worlds/{name}/summary", Name: "world summary", MimeType: "text/plain",
		Description: "The describe_world text for a world",
		Match: func(uri string) (func(context.Context) (string, error), bool) {
			name, ok := strings.CutPrefix(uri, "simo://worlds/")
			name, ok2 := strings.CutSuffix(name, "/summary")
			if !ok || !ok2 || !worldNameRe.MatchString(name) {
				return nil, false
			}
			return func(ctx context.Context) (string, error) {
				return s.locked(s.describeWorld)(ctx, mustArgs(map[string]any{"world": name}))
			}, true
		},
	})
	m.ListResources = func() []mcp.Resource {
		var out []mcp.Resource
		names := s.worldNames()
		if s.cfg.World != "" {
			names = []string{s.cfg.World}
		}
		for _, n := range names {
			n := n
			out = append(out, mcp.Resource{
				URI: "simo://worlds/" + n + "/summary", Name: n + " summary", MimeType: "text/plain",
				Description: "What can be observed in world " + n,
				Read: func(ctx context.Context) (string, error) {
					return s.locked(s.describeWorld)(ctx, mustArgs(map[string]any{"world": n}))
				},
			})
		}
		return out
	}
}

func mustArgs(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (s *server) describeWorld(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		World string `json:"world"`
	}
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	st, err := s.world(a.World)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := report.DescribeWorld(st, &b); err != nil {
		return "", err
	}
	return b.String(), nil
}

func devicesMatching(w *ir.World, pattern string) ([]uint32, error) {
	if pattern == "" {
		return nil, nil
	}
	prefix, isPrefix := strings.CutSuffix(pattern, "*")
	var ids []uint32
	for _, d := range w.Devices {
		if d.Name == pattern || (isPrefix && strings.HasPrefix(d.Name, prefix)) {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no device matches %q", pattern)
	}
	return ids, nil
}

func (s *server) listDevices(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		World  string `json:"world"`
		Class  string `json:"class"`
		Site   int    `json:"site"`
		Name   string `json:"name"`
		Limit  int    `json:"limit"`
		Cursor string `json:"cursor"`
	}
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	st, err := s.world(a.World)
	if err != nil {
		return "", err
	}
	w := st.World
	limit := clamp(a.Limit, defDeviceRows, maxDeviceRows)
	offset := 0
	if a.Cursor != "" {
		if _, err := fmt.Sscanf(a.Cursor, "devices:%d", &offset); err != nil || offset < 0 {
			return "", errors.New("bad cursor")
		}
	}
	prefix, isPrefix := strings.CutSuffix(a.Name, "*")
	var match []ir.Device
	for _, d := range w.Devices {
		if a.Class != "" && w.Classes[d.Class].Name != a.Class {
			continue
		}
		if a.Site != 0 && d.Site != a.Site {
			continue
		}
		if a.Name != "" && d.Name != a.Name && !(isPrefix && strings.HasPrefix(d.Name, prefix)) {
			continue
		}
		match = append(match, d)
	}
	if offset > len(match) {
		offset = len(match)
	}
	page := match[offset:min(offset+limit, len(match))]
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d of %d matching devices\n", len(page), len(match))
	report.DevicesTable(w, page, &b)
	if offset+len(page) < len(match) {
		fmt.Fprintf(&b, "next_cursor: devices:%d\n", offset+len(page))
	}
	return b.String(), nil
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

func (s *server) summarizeLogs(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		World, From, To, For, Step, Device string
		By                                 []string `json:"by"`
	}
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	st, err := s.world(a.World)
	if err != nil {
		return "", err
	}
	w := st.World
	from, to, err := report.Window(w, a.From, a.To, a.For)
	if err != nil {
		return "", err
	}
	q := store.RollupQuery{By: a.By, FromMin: store.MinuteOf(from * w.TickNs), ToMin: store.MinuteOf(to*w.TickNs + int64(time.Minute) - 1)}
	if len(q.By) == 0 {
		q.By = []string{"event"}
	}
	if a.Step != "" {
		d, err := spec.ParseDuration(a.Step)
		if err != nil || d < time.Minute {
			return "", errors.New("step: want at least 1m")
		}
		q.StepMin = int64(d / time.Minute)
	}
	if q.Devices, err = devicesMatching(w, a.Device); err != nil {
		return "", err
	}
	rows, err := st.Rollup(q)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if len(rows) == 0 {
		fmt.Fprintf(&b, "no events in the window (counts cover %s to %s)\n",
			report.Offset(time.Duration(st.BuildFrom)*w.Tick), report.Offset(time.Duration(st.BuildTo)*w.Tick))
		return b.String(), nil
	}
	shown := rows[:min(len(rows), maxRollupRows)]
	report.RollupTable(w, q.By, shown, &b)
	if len(rows) > len(shown) {
		fmt.Fprintf(&b, "... %d more rows; narrow the window, use a larger step or fewer group keys\n", len(rows)-len(shown))
	}
	return b.String(), nil
}

// searchCursor carries a search across pages.
type searchCursor struct {
	World    string `json:"w,omitempty"`
	FromNs   int64  `json:"f"`
	ToNs     int64  `json:"t"`
	Skip     int    `json:"s,omitempty"` // records at FromNs already returned
	Device   string `json:"d,omitempty"`
	Level    string `json:"l,omitempty"`
	Event    string `json:"e,omitempty"`
	Contains string `json:"c,omitempty"`
}

func (c searchCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (searchCursor, error) {
	var c searchCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		return c, errors.New("bad cursor")
	}
	return c, nil
}

func (s *server) searchLogs(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		World, From, To, For, Device, Level, Event, Contains, Cursor string
		Limit                                                        int
	}
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	var c searchCursor
	if a.Cursor != "" {
		var err error
		if c, err = decodeCursor(a.Cursor); err != nil {
			return "", err
		}
	} else {
		c = searchCursor{World: a.World, Device: a.Device, Level: a.Level, Event: a.Event, Contains: a.Contains}
	}
	st, err := s.world(c.World)
	if err != nil {
		return "", err
	}
	w := st.World
	if a.Cursor == "" {
		from, to, err := report.Window(w, a.From, a.To, a.For)
		if err != nil {
			return "", err
		}
		c.FromNs, c.ToNs = from*w.TickNs, to*w.TickNs
	}
	q := store.Query{FromNs: c.FromNs, ToNs: c.ToNs, Workers: s.cfg.Workers}
	if q.Devices, err = devicesMatching(w, c.Device); err != nil {
		return "", err
	}
	if c.Level != "" {
		l := spec.LevelIndex(c.Level)
		if l < 0 {
			return "", fmt.Errorf("unknown level %q", c.Level)
		}
		q.MinLevel = uint8(l)
	}
	if c.Event != "" {
		for i, e := range w.Events {
			if e.ID == c.Event {
				q.Templates = append(q.Templates, uint16(i))
			}
		}
		if q.Templates == nil {
			return "", fmt.Errorf("no event %q (describe_world lists them)", c.Event)
		}
	}
	rend, err := render.New(w)
	if err != nil {
		return "", err
	}
	limit := clamp(a.Limit, defSearchLines, maxSearchLines)
	var lines []string
	var buf []byte
	skip := c.Skip
	lastTS, atLast := int64(-1), 0
	more := false
	_, err = st.Events(q, func(r *record.Record) error {
		if c.Contains != "" {
			buf = rend.Message(buf[:0], r)
			if !strings.Contains(string(buf), c.Contains) {
				return nil
			}
		}
		if r.TS == c.FromNs && skip > 0 {
			skip--
			return nil
		}
		if len(lines) == limit {
			more = true
			return store.ErrStop
		}
		buf = rend.Line(buf[:0], r, render.Text)
		lines = append(lines, string(buf))
		if r.TS == lastTS {
			atLast++
		} else {
			lastTS, atLast = r.TS, 1
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if len(lines) == 0 {
		b.WriteString("no matching lines\n")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "%d lines\n", len(lines))
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if more {
		next := c
		next.FromNs, next.Skip = lastTS, atLast
		if lastTS == c.FromNs {
			next.Skip += c.Skip
		}
		fmt.Fprintf(&b, "next_cursor: %s\n", next.encode())
	} else {
		b.WriteString("end of results\n")
	}
	return b.String(), nil
}

func (s *server) getMetrics(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		World, From, To, For, Device, Metric, Step string
	}
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	st, err := s.world(a.World)
	if err != nil {
		return "", err
	}
	w := st.World
	ids, err := devicesMatching(w, a.Device)
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", errors.New("device: name exactly one device")
	}
	dev := &w.Devices[ids[0]]
	cls := &w.Classes[dev.Class]
	step := time.Duration(5 * time.Minute)
	if a.Step != "" {
		if step, err = spec.ParseDuration(a.Step); err != nil || step < time.Minute {
			return "", errors.New("step: want at least 1m")
		}
	}
	from, to, err := report.Window(w, a.From, a.To, a.For)
	if err != nil {
		return "", err
	}
	fromMin, toMin := store.MinuteOf(from*w.TickNs), store.MinuteOf(to*w.TickNs+int64(time.Minute)-1)
	var series []report.MetricSeries
	total := 0
	for mi, m := range cls.Metrics {
		if a.Metric != "" && m.Name != a.Metric {
			continue
		}
		rows, err := st.Metrics(dev.ID, mi, fromMin, toMin, int64(step/time.Minute))
		if err != nil {
			return "", err
		}
		total += len(rows)
		series = append(series, report.MetricSeries{Name: m.Name, Unit: m.Unit, Rows: rows})
	}
	if len(series) == 0 {
		names := make([]string, len(cls.Metrics))
		for i, m := range cls.Metrics {
			names[i] = m.Name
		}
		return "", fmt.Errorf("%s has no metric %q (it has: %s)", dev.Name, a.Metric, strings.Join(names, ", "))
	}
	if total > maxMetricRows {
		return "", fmt.Errorf("%d rows is too many; use a larger step or a shorter window", total)
	}
	var b bytes.Buffer
	report.MetricsTable(w, series, &b)
	return b.String(), nil
}

func (s *server) listChanges(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		World, From, To, For, Device string
		Limit                        int
	}
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	st, err := s.world(a.World)
	if err != nil {
		return "", err
	}
	w := st.World
	from, to, err := report.Window(w, a.From, a.To, a.For)
	if err != nil {
		return "", err
	}
	ids, err := devicesMatching(w, a.Device)
	if err != nil {
		return "", err
	}
	limit := clamp(a.Limit, 100, maxChangeRows)
	rows, err := st.DB().QueryContext(ctx, `SELECT tick, device, cause FROM transitions
		WHERE tick >= ? AND tick < ? AND (cause LIKE 'operator:%' OR cause LIKE 'command:%') ORDER BY tick, device`, from, to)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var tick int64
		var dev uint32
		var cause string
		if err := rows.Scan(&tick, &dev, &cause); err != nil {
			return "", err
		}
		if ids != nil && !slices.Contains(ids, dev) {
			continue
		}
		if n == limit {
			b.WriteString("... more; narrow the window\n")
			break
		}
		who, what, _ := strings.Cut(cause, ":")
		actor := map[string]string{"operator": "on-call operator", "command": "manual command"}[who]
		fmt.Fprintf(&b, "%s %s %s ran %s\n", w.Start.Add(time.Duration(tick)*w.Tick).Format(time.RFC3339), w.Devices[dev].Name, actor, what)
		n++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if n == 0 {
		return "no operator actions in the window\n", nil
	}
	return b.String(), nil
}

// --- author tools ------------------------------------------------------

func exampleNames() []string {
	entries, _ := fs.ReadDir(examples.FS, ".")
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			names = append(names, n)
		}
	}
	return names
}

// exampleSummary is the first paragraph of an example's leading comment.
func exampleSummary(src []byte) string {
	var parts []string
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if text == "" {
			break
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " ")
}

func (s *server) registerAuthor(m *mcp.Server) {
	m.AddTool(mcp.Tool{
		Name: "get_seed_schema", Title: "Seed JSON Schema", ReadOnly: true,
		Description: "The JSON Schema of the seed format, with descriptions of every field, law and generator. Seeds are written in YAML.",
		InputSchema: object(nil),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			return string(spec.SchemaJSON), mcp.Args(raw, &struct{}{})
		},
	})
	m.AddTool(mcp.Tool{
		Name: "list_examples", Title: "List example seeds", ReadOnly: true,
		Description: "Example seeds with a one-line description each. Read one with get_example.",
		InputSchema: object(nil),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			if err := mcp.Args(raw, &struct{}{}); err != nil {
				return "", err
			}
			var b strings.Builder
			for _, n := range exampleNames() {
				src, _ := examples.FS.ReadFile(n + ".yaml")
				fmt.Fprintf(&b, "%s: %s\n", n, exampleSummary(src))
			}
			return b.String(), nil
		},
	})
	m.AddTool(mcp.Tool{
		Name: "get_example", Title: "Get an example seed", ReadOnly: true,
		Description: "The YAML of an example seed, with comments.",
		InputSchema: object(map[string]any{"name": str("Example name from list_examples")}, "name"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct{ Name string }
			if err := mcp.Args(raw, &a); err != nil {
				return "", err
			}
			src, err := examples.FS.ReadFile(a.Name + ".yaml")
			if err != nil || strings.ContainsAny(a.Name, "/\\.") {
				return "", fmt.Errorf("no example %q (have: %s)", a.Name, strings.Join(exampleNames(), ", "))
			}
			return string(src), nil
		},
	})
	m.AddTool(mcp.Tool{
		Name: "validate_seed", Title: "Validate a seed", ReadOnly: true,
		Description: "Check a seed (YAML) and lint it. Reports every error and warning with its YAML path, or a summary when the seed is valid. Repeat until it reports valid.",
		InputSchema: object(map[string]any{"seed": str("The seed, as YAML text")}, "seed"),
		Handler:     s.validateSeed,
	})
	m.AddTool(mcp.Tool{
		Name: "preview_seed", Title: "Preview a seed", ReadOnly: true,
		Description: "Simulate a short window of a seed and summarise it: counts per event, time in each state, and sample log lines. Use it to check that the world looks right before create_world.",
		InputSchema: object(map[string]any{
			"seed":  str("The seed, as YAML text"),
			"from":  propFrom,
			"to":    propTo,
			"for":   str("Window length, default 1h"),
			"lines": integer(fmt.Sprintf("Sample lines, default %d, max 100", defPreviewLines)),
		}, "seed"),
		Handler: s.previewSeed,
	})
	m.AddTool(mcp.Tool{
		Name: "create_world", Title: "Create a world",
		Description: "Simulate a seed into a new world that observer tools can read. Optionally store raw events for whole days (materialize); other windows are regenerated on read.",
		InputSchema: object(map[string]any{
			"name":        str("World name: lowercase letters, digits, - and _"),
			"seed":        str("The seed, as YAML text"),
			"from":        propFrom,
			"to":          propTo,
			"for":         propFor,
			"materialize": map[string]any{"type": "boolean", "description": "Store raw events for every whole day in the window"},
		}, "name", "seed"),
		Handler: s.locked(s.createWorld),
	})
	m.AddResource(mcp.Resource{
		URI: "simo://schema/seed", Name: "seed schema", MimeType: "application/schema+json",
		Description: "JSON Schema of the seed format",
		Read:        func(context.Context) (string, error) { return string(spec.SchemaJSON), nil },
	})
	for _, n := range exampleNames() {
		n := n
		src, _ := examples.FS.ReadFile(n + ".yaml")
		m.AddResource(mcp.Resource{
			URI: "simo://examples/" + n, Name: "example " + n, MimeType: "application/yaml",
			Description: exampleSummary(src),
			Read:        func(context.Context) (string, error) { return string(src), nil },
		})
	}
}

// compileSeed parses, validates and compiles seed text. It returns the
// issues text when the seed is invalid.
func compileSeed(src string) (*ir.World, spec.Issues, error) {
	s, err := spec.Parse([]byte(src))
	if err != nil {
		return nil, nil, err
	}
	issues := spec.Validate(s)
	if len(issues.Errors()) > 0 {
		return nil, issues, nil
	}
	w, err := compile.Compile(s, []byte(src))
	return w, issues, err
}

func issuesText(issues spec.Issues) string {
	var b strings.Builder
	errs := len(issues.Errors())
	if errs > 0 {
		fmt.Fprintf(&b, "invalid: %s, %s\n", report.Plural(errs, "error"), report.Plural(len(issues)-errs, "warning"))
	}
	for _, is := range issues {
		b.WriteString(is.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func (s *server) validateSeed(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Seed string }
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	w, issues, err := compileSeed(a.Seed)
	if err != nil {
		return "invalid: " + err.Error() + "\n", nil
	}
	if w == nil {
		return issuesText(issues), nil
	}
	var b strings.Builder
	end := time.Duration(w.Ticks) * w.Tick
	perDay := 0.0
	for _, d := range w.Devices {
		for _, e := range w.Classes[d.Class].Events {
			perDay += w.Events[e].PerSec * d.RateScale * 86400
		}
	}
	fmt.Fprintf(&b, "valid: %s, %s, %s, tick %s, %s; about %s rate-driven lines per simulated day before state and pattern effects\n",
		w.Name, report.Plural(len(w.Classes), "class"), report.Plural(len(w.Devices), "device"), w.Tick, report.Span(end), report.Commas(int64(perDay)))
	b.WriteString(issuesText(issues))
	return b.String(), nil
}

func (s *server) checkCost(w *ir.World, toTick int64) error {
	cost := toTick * int64(len(w.Devices))
	if cost > s.cfg.MaxDeviceTicks {
		return fmt.Errorf("that window means simulating %s device-ticks from the start, over this server's limit of %s; use a shorter window or fewer devices",
			report.Commas(cost), report.Commas(s.cfg.MaxDeviceTicks))
	}
	return nil
}

func (s *server) previewSeed(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Seed, From, To, For string
		Lines               int
	}
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	w, issues, err := compileSeed(a.Seed)
	if err != nil {
		return "invalid: " + err.Error() + "\n", nil
	}
	if w == nil {
		return issuesText(issues), nil
	}
	if a.For == "" && a.To == "" {
		a.For = "1h"
	}
	from, to, err := report.Window(w, a.From, a.To, a.For)
	if err != nil {
		return "", err
	}
	if err := s.checkCost(w, to); err != nil {
		return "", err
	}
	var b bytes.Buffer
	b.WriteString(issuesText(issues))
	err = report.Preview(w, report.PreviewOptions{From: from, To: to, Lines: clamp(a.Lines, defPreviewLines, 100), Format: render.Text, Workers: s.cfg.Workers}, &b)
	return b.String(), err
}

func (s *server) createWorld(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Name, Seed, From, To, For string
		Materialize               bool
	}
	if err := mcp.Args(raw, &a); err != nil {
		return "", err
	}
	if !worldNameRe.MatchString(a.Name) {
		return "", errors.New("name: use 1-64 lowercase letters, digits, - and _, starting with a letter or digit")
	}
	dir := filepath.Join(s.cfg.WorldsDir, a.Name)
	if _, err := os.Stat(dir); err == nil {
		return "", fmt.Errorf("world %q already exists", a.Name)
	}
	w, issues, err := compileSeed(a.Seed)
	if err != nil {
		return "", err
	}
	if w == nil {
		return "", errors.New(issuesText(issues))
	}
	from, to, err := report.Window(w, a.From, a.To, a.For)
	if err != nil {
		return "", err
	}
	if err := s.checkCost(w, to); err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.cfg.WorldsDir, 0o755); err != nil {
		return "", err
	}
	started := time.Now()
	res, err := store.Build(dir, w, []byte(a.Seed), store.BuildOptions{From: from, To: to, Materialize: a.Materialize, Workers: s.cfg.Workers})
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "created world %s in %s: %s records over %s, %s stored\n\n", a.Name, time.Since(started).Round(100*time.Millisecond),
		report.Commas(res.Stats.Records), report.Span(time.Duration(to-from)*w.Tick), report.Plural(len(res.MaterializedDays), "day"))
	st, err := s.world(a.Name)
	if err != nil {
		return "", err
	}
	report.DescribeWorld(st, &b)
	return b.String(), nil
}

// --- admin -------------------------------------------------------------

func (s *server) registerAdmin(m *mcp.Server) {
	m.AddTool(mcp.Tool{
		Name: "get_truth", Title: "Ground truth", ReadOnly: true,
		Description: "The answer key: scheduled faults with cause id, time window, target and number of devices. Never give this to an agent under test.",
		InputSchema: object(map[string]any{"world": propWorld}),
		Handler: s.locked(func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct{ World string }
			if err := mcp.Args(raw, &a); err != nil {
				return "", err
			}
			st, err := s.world(a.World)
			if err != nil {
				return "", err
			}
			var b bytes.Buffer
			err = report.TruthTable(st.World, &b)
			return b.String(), err
		}),
	})
}
