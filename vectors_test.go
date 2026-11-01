package shapebundle

import (
	"encoding/json"
	"os"
	"testing"
)

// The vectors in testdata/vectors are the zmux shared shaping test vectors
// (Apache-2.0), copied unchanged. Each case is wrapped as a part of the
// default kind of a bundle, which must then load exactly when the case
// does.
type vectorCase struct {
	Name     string          `json:"name"`
	JSON     json.RawMessage `json:"json"`
	Policy   json.RawMessage `json:"policy"`
	Limits   json.RawMessage `json:"limits"`
	Raw      *string         `json:"raw"`
	Sequence *string         `json:"sequence"`
	OK       *bool           `json:"ok"`
	Error    *string         `json:"error"` // with no ok: absent when the case loads
}

func loadVectors(t *testing.T, file, list string) []vectorCase {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var cs []vectorCase
	if err := json.Unmarshal(m[list], &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestSharedVectors(t *testing.T) {
	for _, v := range []struct{ file, list, section string }{
		{"contract.json", "definitions", "definition"},
		{"pool_policy.json", "parse", "pool"},
		{"session_limits.json", "cases", "session"},
	} {
		n := 0
		for _, c := range loadVectors(t, v.file, v.list) {
			var body string
			switch {
			case c.Sequence != nil:
				continue // not a JSON document
			case c.Raw != nil:
				body = *c.Raw
			case c.JSON != nil:
				body = string(c.JSON)
			case c.Policy != nil:
				body = string(c.Policy)
			case c.Limits != nil:
				body = string(c.Limits)
			default:
				continue
			}
			n++
			doc := `{"version":1,"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":{"` + v.section + `":` + body + `}}}`
			_, err := Parse([]byte(doc), ParseOptions{})
			want := c.Error == nil
			if c.OK != nil {
				want = *c.OK
			}
			if (err == nil) != want {
				t.Errorf("%s %s: want ok %v, error %v", v.file, c.Name, want, err)
			}
		}
		t.Logf("%s: %d cases", v.file, n)
	}
}
