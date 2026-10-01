package engine_test

import (
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/spec"
)

var update = flag.Bool("update", false, "rewrite testdata/golden hashes")

// Golden hashes pin the record stream of every example seed. A change here
// means the engine's output changed: if that is intended, bump
// ir.EngineVersion and run `go test ./internal/engine -run Golden -update`.
func TestGoldenHashes(t *testing.T) {
	cases := []struct {
		seed  string
		ticks int64 // 0 = whole run
	}{
		{"retail-pos.yaml", 12 * 3600},
		{"tcp-connections.yaml", 0},
		{"k8s-web-api.yaml", 12 * 3600},
	}
	for _, c := range cases {
		t.Run(c.seed, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "examples", c.seed))
			if err != nil {
				t.Fatal(err)
			}
			s, err := spec.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			w, err := compile.Compile(s, raw)
			if err != nil {
				t.Fatal(err)
			}
			recs, _, _ := runAll(t, w, engine.Options{To: c.ticks})
			sum := hashRecords(recs)
			got := hex.EncodeToString(sum[:])
			path := filepath.Join("..", "..", "testdata", "golden", strings.TrimSuffix(c.seed, ".yaml")+".sha256")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				content := got + "  engine " + ir.EngineVersion + ", " + c.seed + "\n"
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no golden hash (run with -update): %v", err)
			}
			if fields := strings.Fields(string(want)); len(fields) == 0 || fields[0] != got {
				t.Errorf("record stream changed: sha256 %s, golden %s (%d records)", got, strings.TrimSpace(string(want)), len(recs))
			}
		})
	}
}
