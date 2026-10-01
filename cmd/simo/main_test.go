package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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
