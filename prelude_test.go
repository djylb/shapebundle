package shapebundle

import (
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/zmuxio/zmux-go"
	"github.com/zmuxio/zmux-go/shaping/machine"
)

func preludeBundle(prelude string) []byte {
	return []byte(`{"version":1,"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":{"prelude":` + prelude + `}}}`)
}

func TestPrelude(t *testing.T) {
	const good = `{"messages":[{"size":{"range":[900,1200]},"delay":{"const":0}},{"size":{"const":500},"delay":{"range":[0.001,0.002]}},{"size":{"const":300}}]}`
	b, err := Parse(preludeBundle(good), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := b.Default()
	if e.Prelude == nil || len(e.Prelude.Messages) != 3 || e.Prelude.Messages[1].Delay == nil || e.Prelude.Messages[2].Delay != nil {
		t.Fatalf("%+v", e.Prelude)
	}
	p, err := NewProfile(ProfileOptions{}, b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Prelude(zmux.CarrierKind(1)) != e.Prelude {
		t.Fatal("profile resolved another prelude")
	}

	for _, src := range []string{`null`, `{}`, `{"messages":[{},{},{},{},{"size":{"const":1}}]}`} {
		if _, err := Parse(preludeBundle(src), ParseOptions{}); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
	for _, src := range []string{
		`{"knob":1}`,
		`{"messages":[{"size":{"const":1},"pad":{"const":1}}]}`,
		`{"messages":[{"size":{"const":0}}]}`,
		`{"messages":[{"size":{"const":65536}}]}`,
		`{"messages":[{"delay":{"const":-1}}]}`,
		`{"messages":[{},{},{},{"delay":{"const":-1}}]}`,
	} {
		if _, err := Parse(preludeBundle(src), ParseOptions{}); err == nil || !strings.Contains(err.Error(), "prelude") {
			t.Errorf("%s: %v", src, err)
		}
	}
	bag := `{"messages":[{"size":{"dist":{"kind":"bag","points":[1,2,3]}}}]}`
	if _, err := Parse(preludeBundle(bag), ParseOptions{Size: machine.SizeLimits{MaxPoints: 2}}); err == nil {
		t.Error("points over the bound accepted")
	}

	// A member the format does not have is still refused beside it.
	raw := `{"version":1,"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":{"prelude":{},"other":{}}}}`
	if _, err := Parse([]byte(raw), ParseOptions{}); err == nil {
		t.Error("unknown member accepted")
	}

	// No prelude in the bundle: none in the profile.
	b, err = Parse([]byte(`{"version":1,"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":{}}}`), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p, err = NewProfile(ProfileOptions{}, b); err != nil || p.Prelude(0) != nil {
		t.Fatalf("%v, %+v", err, p.Prelude(0))
	}
	var empty Profile
	if empty.Prelude(0) != nil {
		t.Fatal("empty profile has a prelude")
	}
}

// TestPreludeOfBundleFile parses a bundle written by a generator, named by
// SHAPEBUNDLE_TEST_FILE, and draws from every prelude in it.
func TestPreludeOfBundleFile(t *testing.T) {
	path := os.Getenv("SHAPEBUNDLE_TEST_FILE")
	if path == "" {
		t.Skip("SHAPEBUNDLE_TEST_FILE not set")
	}
	b, err := ParseFile(path, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := machine.NewSampler(rand.NewPCG(1, 2))
	entries := map[string]*Entry{}
	for _, name := range b.Kinds() {
		k, known := kindByName[name]
		if known {
			entries[name], _ = b.Lookup(k)
		} else {
			entries[name], _ = b.Default()
		}
	}
	if len(entries) == 0 {
		t.Fatal("no kinds")
	}
	for name, e := range entries {
		if e.Prelude == nil || len(e.Prelude.Messages) != 3 {
			t.Fatalf("%s: prelude %+v", name, e.Prelude)
		}
		for seq, m := range e.Prelude.Messages {
			if m.Size == nil {
				t.Fatalf("%s: message %d has no size", name, seq)
			}
			lo, hi := m.Size.Bounds()
			if lo < 1 || hi > machine.MaxPreludeSize {
				t.Fatalf("%s: message %d: size bounds [%v, %v]", name, seq, lo, hi)
			}
			for range 200 {
				x := s.Sample(m.Size)
				if x < lo || x > hi {
					t.Fatalf("%s: message %d: drew %v", name, seq, x)
				}
				pad, d := e.Prelude.Shape(s, seq, 1)
				if pad < 0 || pad+1 > int(hi+0.5) || d < 0 {
					t.Fatalf("%s: message %d: shape %d, %v", name, seq, pad, d)
				}
			}
		}
	}
}
