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
	if got := strings.Join(b.Kinds(), ","); got != "websocket,quic,split,default" {
		t.Errorf("kinds %s", got)
	}
	ws := b.Entry(zmux.CarrierWebSocket)
	if ws == nil || ws.Definition == nil || ws.Session == nil || ws.Pool == nil {
		t.Fatalf("websocket entry %+v", ws)
	}
	if ws.Session.MaxPaddingRatio != 0.1 || ws.Session.MaxInjected != 100 || ws.Session.MaxDelay != 0 {
		t.Errorf("session %+v", *ws.Session)
	}
	if e := b.Entry(zmux.CarrierSplit); e == nil || e.Definition != nil || e.Session != nil || e.Pool != nil {
		t.Errorf("split entry %+v: a listed empty kind does not fall back", e)
	}
	def, _ := b.Default()
	if b.Entry(zmux.CarrierHTTP3) != def || b.Entry(0) != def || def == nil || !def.Session.NoPadding {
		t.Errorf("default fallback")
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

func TestParseErrors(t *testing.T) {
	const g = `"generated_at":"2026-01-01T00:00:00Z"`
	const k = `"kinds":{"default":{}}`
	cases := map[string]string{
		"unknown root":       `{"version":1,` + g + `,` + k + `,"extra":1}`,
		"version 2":          `{"version":2,` + g + `,` + k + `}`,
		"version 1.0":        `{"version":1.0,` + g + `,` + k + `}`,
		"version 1e0":        `{"version":1e0,` + g + `,` + k + `}`,
		"version string":     `{"version":"1",` + g + `,` + k + `}`,
		"version null":       `{"version":null,` + g + `,` + k + `}`,
		"no version":         `{` + g + `,` + k + `}`,
		"no generated_at":    `{"version":1,` + k + `}`,
		"bad generated_at":   `{"version":1,"generated_at":"yesterday",` + k + `}`,
		"generated_at num":   `{"version":1,"generated_at":5,` + k + `}`,
		"no kinds":           `{"version":1,` + g + `}`,
		"kinds null":         `{"version":1,` + g + `,"kinds":null}`,
		"kinds empty":        `{"version":1,` + g + `,"kinds":{}}`,
		"kinds all null":     `{"version":1,` + g + `,"kinds":{"default":null}}`,
		"kinds array":        `{"version":1,` + g + `,"kinds":[]}`,
		"unknown kind":       `{"version":1,` + g + `,"kinds":{"tcp":{}}}`,
		"kind case":          `{"version":1,` + g + `,"kinds":{"WebSocket":{}}}`,
		"member case":        `{"Version":1,` + g + `,` + k + `}`,
		"unknown kind field": `{"version":1,` + g + `,"kinds":{"default":{"limits":{}}}}`,
		"duplicate root":     `{"version":1,"version":1,` + g + `,` + k + `}`,
		"duplicate kind":     `{"version":1,` + g + `,"kinds":{"default":{},"default":{}}}`,
		"duplicate part":     `{"version":1,` + g + `,"kinds":{"default":{"session":{},"session":{}}}}`,
		"bad definition":     `{"version":1,` + g + `,"kinds":{"default":{"definition":{"version":1,"machines":[]}}}}`,
		"definition extra":   `{"version":1,` + g + `,"kinds":{"default":{"definition":{"version":1,"machines":[{"states":[{}]}],"x":1}}}}`,
		"bad session":        `{"version":1,` + g + `,"kinds":{"default":{"session":{"max_padding_ratio":2}}}}`,
		"session fraction":   `{"version":1,` + g + `,"kinds":{"default":{"session":{"max_injected":1.0}}}}`,
		"bad pool":           `{"version":1,` + g + `,"kinds":{"default":{"pool":{"count":{"const":0}}}}}`,
		"pool huge":          `{"version":1,` + g + `,"kinds":{"default":{"pool":{"count":{"const":1e400}}}}}`,
		"trailing":           `{"version":1,` + g + `,` + k + `} {}`,
		"not object":         `[]`,
		"empty":              ``,
		"truncated":          `{"version":1,`,
		"bad utf8":           "{\"version\":1," + g + `,"kinds":{"default":{}},"\xff":1}`,
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in), ParseOptions{}); err == nil {
			t.Errorf("%s: no error", name)
		}
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
		t.Error("definition MaxBytes not applied")
	}
	big := `{"version":1,"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":{"pool":` +
		`{"count":{"dist":{"kind":"empirical","points":[1,2,3],"weights":[1,1,1]}}}}}}`
	if _, err := Parse([]byte(big), ParseOptions{Size: machine.SizeLimits{MaxPoints: 2}}); err == nil {
		t.Error("pool MaxPoints not applied")
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(bundleJSON("2026-01-02T00:00:00Z")))
	f.Add([]byte(`{"version":1,"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":{}}}`))
	f.Add([]byte(`{"version":1,"version":1}`))
	f.Add([]byte(`{"kinds":{"quic":null,"default":{"session":null}}}`))
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
