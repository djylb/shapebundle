package shapebundle

import (
	"strings"
	"testing"
	"time"

	"github.com/zmuxio/zmux-go"
	"github.com/zmuxio/zmux-go/shaping/machine"
)

func TestParseValid(t *testing.T) {
	b, err := Parse([]byte(bundleJSON("2026-01-02T00:00:00+01:00")), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.GeneratedAt().Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("generated_at %v", b.GeneratedAt())
	}
	if !b.NextChange().Equal(time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("next_change %v", b.NextChange())
	}
	if got := strings.Join(b.Kinds(), ","); got != "default,websocket,split,quic" {
		t.Errorf("kinds %s", got)
	}
	ws := b.Entry(zmux.CarrierWebSocket)
	if ws == nil || !ws.Candidate.HasMachines() || !ws.Candidate.HasPool() {
		t.Fatalf("websocket entry %+v", ws)
	}
	if l, ok := ws.Candidate.Limits(); !ok || l.MaxPaddingPPM != 100000 || l.MaxInjected != 100 || l.MaxDelay != 0 {
		t.Errorf("limits %+v", l)
	}
	if e := b.Entry(zmux.CarrierSplit); e == nil || e.Candidate.HasMachines() || e.Candidate.HasPool() {
		t.Errorf("split entry %+v: a listed empty kind does not fall back", e)
	}
	def, _ := b.Default()
	if b.Entry(zmux.CarrierHTTP3) != def || b.Entry(0) != def || def == nil {
		t.Errorf("default fallback")
	}
	if l, _ := def.Candidate.Limits(); !l.NoPadding {
		t.Errorf("default limits")
	}
	if _, ok := b.Lookup(zmux.CarrierHTTP3); ok {
		t.Errorf("Lookup falls back")
	}
	if !strings.Contains(string(b.Raw()), `"quic"`) {
		t.Errorf("raw")
	}
}

func TestKindNames(t *testing.T) {
	for _, n := range kindNames {
		k, ok := kindByName[n]
		if !ok || KindName(k) != n {
			t.Errorf("kind %s does not map to a zmux carrier kind", n)
		}
	}
	if KindName(0) != DefaultKind {
		t.Errorf("KindName(0)")
	}
}

func TestParseBounds(t *testing.T) {
	in := []byte(bundleJSON(""))
	if _, err := Parse(in, ParseOptions{MaxBytes: len(in) - 1}); err == nil {
		t.Error("MaxBytes not applied")
	}
	if _, err := Parse(in, ParseOptions{MaxBytes: -1}); err != nil {
		t.Error(err)
	}
	if _, err := Parse(in, ParseOptions{Size: machine.SizeLimits{MaxBytes: 10}}); err == nil {
		t.Error("candidate MaxBytes not applied")
	}
	big := `{"kinds":{"default":[{"pool":{"count":{"t":[[1,1,1],[1,2,2],[1,3,3]]}}}]}}`
	if _, err := Parse([]byte(big), ParseOptions{Size: machine.SizeLimits{MaxSegments: 2}}); err == nil {
		t.Error("MaxSegments not applied")
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(bundleJSON("2026-01-02T00:00:00Z")))
	f.Add([]byte(`{"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":[{}]}}`))
	f.Add([]byte(`{"kinds":{"quic":null,"default":[{"limits":null}]}}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		b, err := Parse(in, ParseOptions{})
		if err != nil {
			return
		}
		// A valid bundle parses again from its own bytes.
		if _, err := Parse(b.Raw(), ParseOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := NewProfile(ProfileOptions{}, b); err != nil {
			t.Fatal(err)
		}
	})
}
