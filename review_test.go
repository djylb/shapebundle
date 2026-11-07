package shapebundle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djylb/timeshim"
	"github.com/zmuxio/zmux-go/shaping"
	"github.com/zmuxio/zmux-go/shaping/bundle"
)

func TestUnlimitedMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(path, []byte(bundleJSON("")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(path, bundle.ParseOptions{MaxBytes: -1}); err != nil {
		t.Errorf("ParseFile: %v", err)
	}
	s := newServer(t, func() string { return bundleJSON("") })
	c := s.config("")
	c.Parse.MaxBytes = -1
	f, _ := NewFetcher(c)
	if _, err := f.Refresh(ctxT(t)); err != nil {
		t.Errorf("Refresh: %v", err)
	}
}

func TestRefreshRejectsOlder(t *testing.T) {
	gen := "2026-01-02T00:00:00Z"
	s := newServer(t, func() string { return strings.Replace(bundleJSON(""), "2026-01-01T00:00:00Z", gen, 1) })
	dir := t.TempDir()
	f, _ := NewFetcher(s.config(dir))
	b, err := f.Refresh(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Refresh(ctxT(t)); err != nil {
		t.Errorf("same generated_at: %v", err)
	}
	cur := f.Current()
	s.set(func(*server) { gen = "2026-01-01T12:00:00Z" })
	if _, err := f.Refresh(ctxT(t)); !errors.Is(err, ErrOlder) {
		t.Errorf("older: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, DefaultCacheName))
	if f.Current() != cur || string(raw) != string(b.Raw()) {
		t.Error("older bundle taken")
	}
}

func TestSessionBudget(t *testing.T) {
	m := `"machines":[{"start":0,"states":[{}]}],`
	mk := func(kinds string) *Profile {
		t.Helper()
		b := mustParse(t, `{"generated_at":"2026-01-01T00:00:00Z","kinds":{`+kinds+`}`)
		p, err := NewProfile(ProfileOptions{}, b)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	f := mk(`"quic":[{` + m + `"budget":{"wire":{"initial":10,"per_mb":100000},"max_delay":2000000}}],` +
		`"default":[{` + m + `"budget":{"wire":{"initial":30,"per_mb":50000},"max_delay":1000000}}]}`).SessionBudget()
	want := shaping.Budget{Wire: shaping.Account{Initial: 30, PerMB: 100000}, MaxDelay: 2 * time.Second}
	if f == nil || f() != want {
		t.Error("widest over the kinds")
	}
	f = mk(`"quic":[{` + m + `"budget":{"wire":{}}}],"default":[{` + m + `"budget":{"wire":{}}}]}`).SessionBudget()
	if f == nil || f() != (shaping.Budget{NoInjection: true}) {
		t.Error("every kind allows no injection")
	}
	f = mk(`"quic":[{` + m + `"budget":{"wire":{}}}],"default":[{` + m + `"budget":{"wire":{"initial":5}}}]}`).SessionBudget()
	if f == nil || f().Wire.Initial != 5 || f().NoInjection {
		t.Error("one kind allowing wins over no injection")
	}
	if mk(`"quic":[{`+m+`"budget":{"wire":{"initial":1}}}],"default":[{`+strings.TrimSuffix(m, ",")+`}]}`).SessionBudget() != nil {
		t.Error("a shaped kind without a budget leaves none")
	}
	f = mk(`"quic":[{` + m + `"budget":{"wire":{"initial":1}}}],"default":[{"pool":{"count":1}}]}`).SessionBudget()
	if f == nil || f().Wire.Initial != 1 {
		t.Error("an unshaped kind adds nothing")
	}
}

func TestFetcherGeneratedAtRules(t *testing.T) {
	clock := timeshim.NewFake(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	gen := "2026-01-02T00:00:00Z"
	s := newServer(t, func() string { return strings.Replace(bundleJSON(""), "2026-01-01T00:00:00Z", gen, 1) })
	dir := t.TempDir()
	cfg := s.config(dir)
	cfg.Clock = clock
	f, _ := NewFetcher(cfg)
	if _, err := f.Refresh(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, DefaultCacheName)
	newer, _ := os.ReadFile(cache)

	// A new Fetcher with nothing current compares with the cache.
	s.set(func(*server) { gen = "2026-01-01T00:00:00Z" })
	g, _ := NewFetcher(cfg)
	if _, err := g.Refresh(ctxT(t)); !errors.Is(err, ErrOlder) {
		t.Errorf("older than the cache: %v", err)
	}
	if raw, _ := os.ReadFile(cache); string(raw) != string(newer) {
		t.Error("cache rolled back")
	}

	// Load keeps a newer current bundle.
	old := strings.Replace(bundleJSON(""), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", 1)
	cur := f.Current()
	if err := os.WriteFile(cache, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); !errors.Is(err, ErrOlder) || f.Current() != cur {
		t.Errorf("Load of an older cache: %v", err)
	}

	// A bundle beyond MaxSkew is refused, and does not block later ones.
	s.set(func(*server) { gen = "2026-01-02T02:00:00Z" })
	if _, err := f.Refresh(ctxT(t)); !errors.Is(err, ErrFuture) {
		t.Errorf("future: %v", err)
	}
	s.set(func(*server) { gen = "2026-01-02T00:30:00Z" })
	if _, err := f.Refresh(ctxT(t)); err != nil {
		t.Errorf("within skew: %v", err)
	}
	// A future cache is ignored as a base and refused by Load.
	if err := os.WriteFile(cache, []byte(strings.Replace(bundleJSON(""), "2026-01-01T00:00:00Z", "2030-01-01T00:00:00Z", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	h, _ := NewFetcher(cfg)
	if _, err := h.Load(); !errors.Is(err, ErrFuture) {
		t.Errorf("Load of a future cache: %v", err)
	}
	if _, err := h.Refresh(ctxT(t)); err != nil {
		t.Errorf("Refresh over a future cache: %v", err)
	}
	clock.Advance(10 * time.Hour)
	s.set(func(*server) { gen = "2026-01-02T11:00:00Z" })
	if _, err := h.Refresh(ctxT(t)); err != nil {
		t.Errorf("skew moves with the clock: %v", err)
	}
}
