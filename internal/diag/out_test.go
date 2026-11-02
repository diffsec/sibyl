package diag

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusListsIncidentsAndBundles(t *testing.T) {
	d, _ := newDiag(t, "http://127.0.0.1:1")
	for _, n := range []string{"20261030T100000Z-manual", "20261030T110000Z-gpu-fault"} {
		if err := os.MkdirAll(filepath.Join(d.IncidentDir(), n), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(d.BundleDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	ids := []string{NewID(), NewID(), NewID()}
	for i, id := range ids {
		p := filepath.Join(d.BundleDir(), id+".tar.gz")
		if err := os.WriteFile(p, make([]byte, 10*(i+1)), 0o640); err != nil {
			t.Fatal(err)
		}
		mt := time.Date(2026, 10, 30, 10+i, 0, 0, 0, time.UTC)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.BundleDir(), ids[0]+".tar.gz.sent"), []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	d.MarkPending(filepath.Join(d.BundleDir(), ids[1]+".tar.gz"))

	if err := d.WriteOut(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(d.OutDir(), "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st StatusFile
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Incidents != 2 || st.LatestIncident != "20261030T110000Z-gpu-fault" ||
		len(st.RecentIncidents) != 2 || st.RecentIncidents[0] != st.LatestIncident {
		t.Fatalf("incidents %d %q %v", st.Incidents, st.LatestIncident, st.RecentIncidents)
	}
	want := []BundleInfo{{ID: ids[2], State: BundleLocal, Bytes: 30}, {ID: ids[1], State: BundlePending, Bytes: 20}, {ID: ids[0], State: BundleSent, Bytes: 10}}
	if len(st.Bundles) != len(want) {
		t.Fatalf("bundles %+v", st.Bundles)
	}
	for i, w := range want {
		if g := st.Bundles[i]; g.ID != w.ID || g.State != w.State || g.Bytes != w.Bytes {
			t.Errorf("bundle %d = %+v, want %+v", i, g, w)
		}
	}
	if st.Consent != ConsentOff || st.SafeMode.On {
		t.Errorf("consent %q, safe mode %v", st.Consent, st.SafeMode)
	}
	if got := d.Bundles(1); len(got) != 1 || got[0].ID != ids[2] {
		t.Errorf("Bundles(1) = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(d.OutDir(), "perf.json")); err != nil {
		t.Error(err)
	}
}
