package compile

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/taituo/simo/internal/spec"
)

func load(t *testing.T, name string) (*spec.Seed, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	s, err := spec.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s, raw
}

func TestFleetExpansion(t *testing.T) {
	s, raw := load(t, "retail-pos.yaml")
	w, err := Compile(s, raw)
	if err != nil {
		t.Fatal(err)
	}
	perSite := map[int]int{}
	nameRe := regexp.MustCompile(`^pos-\d{2}-\d{2}$`)
	names := map[string]bool{}
	for _, d := range w.Devices {
		perSite[d.Site]++
		if !nameRe.MatchString(d.Name) || names[d.Name] {
			t.Errorf("bad or duplicate device name %q", d.Name)
		}
		names[d.Name] = true
		if d.RateScale <= 0 {
			t.Errorf("%s: rate scale %v", d.Name, d.RateScale)
		}
	}
	if len(perSite) != 20 {
		t.Errorf("%d sites, want 20", len(perSite))
	}
	for site, n := range perSite {
		if n < 4 || n > 8 {
			t.Errorf("site %d has %d terminals, want 4..8", site, n)
		}
	}

	// The same seed always expands to the same fleet.
	w2, _ := Compile(s, raw)
	for i := range w.Devices {
		if w.Devices[i].Name != w2.Devices[i].Name || w.Devices[i].RateScale != w2.Devices[i].RateScale {
			t.Fatal("fleet expansion is not deterministic")
		}
	}

	// The fault targets exactly site 7.
	f := w.Faults[0]
	for i, in := range f.Member {
		if in != (w.Devices[i].Site == 7) {
			t.Errorf("%s: fault membership %v", w.Devices[i].Name, in)
		}
	}
	if f.Start != 33*3600+15*60 || f.End != f.Start+40*60 {
		t.Errorf("fault window [%d, %d)", f.Start, f.End)
	}
}

func TestDeviceName(t *testing.T) {
	cases := []struct {
		entry spec.FleetEntry
		site  int
		want  string
	}{
		{spec.FleetEntry{Name: "pos-{site:02}-{n:02}", Sites: 3}, 7, "pos-07-03"},
		{spec.FleetEntry{Name: "conn-{i:04}"}, 0, "conn-0003"},
		{spec.FleetEntry{}, 0, "box-0003"},
		{spec.FleetEntry{Sites: 2}, 1, "box-01-03"},
	}
	for _, c := range cases {
		if got := deviceName(c.entry, "box", c.site, 3, 3); got != c.want {
			t.Errorf("deviceName(%q) = %q, want %q", c.entry.Name, got, c.want)
		}
	}
}

func TestConstantRateThresholds(t *testing.T) {
	s, raw := load(t, "tcp-connections.yaml")
	w, err := Compile(s, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range w.Classes[0].States {
		if len(st.Rates) == 0 {
			continue
		}
		if !st.Const || st.LeaveThr == 0 || st.PickThr[len(st.PickThr)-1] != 1<<32-1 {
			t.Errorf("state %s: thresholds %+v", st.Name, st)
		}
		for i := 1; i < len(st.PickThr); i++ {
			if st.PickThr[i] < st.PickThr[i-1] {
				t.Errorf("state %s: pick thresholds not increasing", st.Name)
			}
		}
	}
}
