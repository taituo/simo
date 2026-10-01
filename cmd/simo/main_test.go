package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	retail = "../../examples/retail-pos.yaml"
	tcp    = "../../examples/tcp-connections.yaml"
)

func simo(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestValidate(t *testing.T) {
	out, _, code := simo(t, "validate", retail)
	if code != 0 || !strings.Contains(out, "ok: retail-pos-fleet") {
		t.Errorf("validate: code %d, output %q", code, out)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("simo: 1\nname: x\nclock: {start: nope, duration: 1h, tick: 1s}\nclasses: {}\nfleet: []\n"), 0o644)
	out, _, code = simo(t, "validate", bad)
	if code != 1 || !strings.Contains(out, "clock.start") {
		t.Errorf("validate bad seed: code %d, output %q", code, out)
	}
}

func TestPreview(t *testing.T) {
	out, errOut, code := simo(t, "preview", tcp, "--for", "2m", "--lines", "3")
	if code != 0 {
		t.Fatalf("preview failed: %s", errOut)
	}
	for _, want := range []string{"tcp-connections: 200 devices", "tcp_conn/data", "time in state:", "sample lines:"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview output lacks %q:\n%s", want, out)
		}
	}
}

func TestRunOutAndLogs(t *testing.T) {
	dir := t.TempDir()
	_, errOut, code := simo(t, "run", retail, "--for", "30m", "--out", dir, "--trace-states", "--metrics", "5m")
	if code != 0 {
		t.Fatalf("run failed: %s", errOut)
	}
	var m Manifest
	mj, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mj, &m); err != nil {
		t.Fatal(err)
	}
	recs, _ := os.ReadFile(filepath.Join(dir, "records.bin"))
	sum := sha256.Sum256(recs)
	if m.Records == 0 || int64(len(recs)) != m.Records*64 || hex.EncodeToString(sum[:]) != m.RecordsSHA256 {
		t.Errorf("manifest %+v does not match records.bin (%d bytes)", m, len(recs))
	}
	for _, f := range []string{"seed.yaml", "truth.jsonl", "transitions.jsonl", "metrics.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	metrics, _ := os.ReadFile(filepath.Join(dir, "metrics.jsonl"))
	if n := bytes.Count(metrics, []byte("\n")); n != 123*6 {
		t.Errorf("metrics.jsonl has %d samples, want 123 devices x 6", n)
	}

	// logs renders the same number of lines as run writes records.
	out, errOut, code := simo(t, "logs", dir)
	if code != 0 {
		t.Fatalf("logs failed: %s", errOut)
	}
	if n := strings.Count(out, "\n"); int64(n) != m.Records {
		t.Errorf("logs printed %d lines, manifest says %d records", n, m.Records)
	}
	out, _, _ = simo(t, "logs", dir, "--level", "WARN", "--format", "jsonl")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil || obj["level"] != "WARN" {
			t.Fatalf("bad --level WARN line %q", line)
		}
	}
	out, _, _ = simo(t, "logs", dir, "--device", "pos-01-*", "--limit", "5")
	if n := strings.Count(out, "\n"); n != 5 || strings.Count(out, " pos-01-") != 5 {
		t.Errorf("--device prefix filter: %q", out)
	}
}

func TestRunToStdoutMatchesLogs(t *testing.T) {
	out, errOut, code := simo(t, "run", tcp, "--for", "1m", "--format", "syslog")
	if code != 0 {
		t.Fatalf("run failed: %s", errOut)
	}
	dir := t.TempDir()
	simo(t, "run", tcp, "--for", "1m", "--out", dir)
	logs, _, _ := simo(t, "logs", dir, "--format", "syslog")
	if out == "" || out != logs {
		t.Error("lines streamed by run differ from lines rendered by logs")
	}
}

func TestUnknownCommand(t *testing.T) {
	if _, _, code := simo(t, "frobnicate"); code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
}

func TestWorldCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tcp")
	_, errOut, code := simo(t, "run", tcp, "--db", dir, "--materialize", "--checkpoint-every", "10m")
	if code != 0 || !strings.Contains(errOut, "raw events stored for day1") {
		t.Fatalf("run --db: code %d, %s", code, errOut)
	}
	if _, _, code := simo(t, "run", tcp, "--db", dir); code != 1 {
		t.Error("building into an existing world did not fail")
	}

	// The same window read from storage and regenerated gives the same lines.
	window := []string{"--from", "00:31", "--for", "7m", "--format", "jsonl"}
	sqlOut, errOut, code := simo(t, append([]string{"logs", dir, "--mode", "sql", "--explain"}, window...)...)
	if code != 0 || !strings.Contains(errOut, "read from stored events") {
		t.Fatalf("logs --mode sql: code %d, %s", code, errOut)
	}
	regenOut, errOut, _ := simo(t, append([]string{"logs", dir, "--mode", "regen", "--explain"}, window...)...)
	if !strings.Contains(errOut, "regenerated from a checkpoint") {
		t.Errorf("logs --mode regen did not regenerate: %s", errOut)
	}
	if sqlOut == "" || sqlOut != regenOut {
		t.Error("stored and regenerated lines differ")
	}

	// stats counts add up to the number of lines.
	all, _, _ := simo(t, "logs", dir)
	stats, _, code := simo(t, "stats", dir, "--by", "level")
	if code != 0 {
		t.Fatal("stats failed")
	}
	total := 0
	for _, line := range strings.Split(strings.TrimSpace(stats), "\n")[1:] {
		f := strings.Fields(line)
		n, _ := strconv.Atoi(strings.ReplaceAll(f[len(f)-1], ",", ""))
		total += n
	}
	if lines := strings.Count(all, "\n"); total != lines || lines == 0 {
		t.Errorf("stats total %d, logs printed %d lines", total, lines)
	}

	out, _, _ := simo(t, "devices", dir)
	if n := strings.Count(out, "\n"); n != 201 {
		t.Errorf("devices printed %d lines, want header + 200", n)
	}
	out, _, _ = simo(t, "truth", dir)
	if !strings.Contains(out, "no scheduled faults") {
		t.Errorf("truth: %q", out)
	}
	_, errOut, _ = simo(t, "materialize", dir)
	if !strings.Contains(errOut, "nothing to do") {
		t.Errorf("materialize on a stored world: %q", errOut)
	}
	if _, errOut, code := simo(t, "metrics", dir, "--device", "conn-0001", "--metric", "temp"); code != 1 || !strings.Contains(errOut, "no metric") {
		t.Errorf("metrics on a class without metrics: code %d, %q", code, errOut)
	}
}

func TestWorldMetricsAndTruth(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "retail")
	if _, errOut, code := simo(t, "run", retail, "--for", "2h", "--db", dir); code != 0 {
		t.Fatalf("run --db: %s", errOut)
	}
	out, _, code := simo(t, "metrics", dir, "--device", "pos-03-01", "--step", "30m")
	if code != 0 || strings.Count(out, "temp_c") != 4 {
		t.Errorf("metrics: code %d, output %q", code, out)
	}
	out, _, _ = simo(t, "truth", dir)
	if !strings.Contains(out, "card reader firmware bug at site 7") || !strings.Contains(out, "day2 09:15:00") {
		t.Errorf("truth: %q", out)
	}
	out, _, _ = simo(t, "stats", dir, "--step", "1h", "--by", "event", "--device", "pos-03-*")
	if strings.Count(out, "pos_terminal/txn_ok") != 2 {
		t.Errorf("stats by hour: %q", out)
	}
	if _, errOut, code := simo(t, "logs", dir, "--from", "day2 09:00", "--for", "1h", "--mode", "sql"); code != 1 || !strings.Contains(errOut, "not materialized") {
		t.Errorf("logs --mode sql outside stored days: code %d, %q", code, errOut)
	}
}

func TestServeStdio(t *testing.T) {
	old := stdin
	defer func() { stdin = old }()
	stdin = strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_examples","arguments":{}}}`,
	}, "\n") + "\n")
	out, errOut, code := simo(t, "serve", "--role", "author", "--worlds", t.TempDir())
	if code != 0 {
		t.Fatalf("serve: code %d, %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout must carry only protocol messages; got %q", out)
	}
	for _, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("not JSON: %s", l)
		}
	}
	if !strings.Contains(lines[0], `"protocolVersion":"2025-06-18"`) || !strings.Contains(lines[1], "tcp-connections") {
		t.Errorf("unexpected responses:\n%s", out)
	}
	if _, errOut, code := simo(t, "serve", "--world", t.TempDir()); code != 1 || !strings.Contains(errOut, "not a world directory") {
		t.Errorf("serve on a non-world: code %d, %s", code, errOut)
	}
}
