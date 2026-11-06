package shapebundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("20-b.json", bundleJSON(""))
	write("10-a.json", `{"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":[{}],"quic":[{}]}}`)
	write(".hidden.json", "garbage")
	write("notes.txt", "garbage")
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDir(dir, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "10-a.json" || got[1].Name != "20-b.json" || len(Bundles(got)) != 2 {
		t.Fatalf("%+v", got)
	}
	write("30-bad.json", `{"version":1}`)
	if _, err := LoadDir(dir, ParseOptions{}); err == nil {
		t.Error("bad file accepted")
	}
	if got, err := LoadDir(filepath.Join(dir, "missing"), ParseOptions{}); got != nil || err != nil {
		t.Error("missing dir", got, err)
	}
}
