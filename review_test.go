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
)

func TestUnlimitedMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(path, []byte(bundleJSON("")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(path, ParseOptions{MaxBytes: -1}); err != nil {
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

func TestSessionLimits(t *testing.T) {
	mk := func(kinds string) *Profile {
		kinds = strings.ReplaceAll(kinds, `{"session":`, `{"definition":`+defA+`,"session":`)
		kinds = strings.ReplaceAll(kinds, `"shaped":{}`, `{"definition":`+defA+`}`)
		p, err := NewProfile(ProfileOptions{}, mustParse(t, `{"version":1,"generated_at":"2026-01-01T00:00:00Z","kinds":{`+kinds+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	l, ok := mk(`"quic":{"session":{"max_padding_bytes":10,"max_padding_ratio":0.1,"max_injected":0,"max_delay":2}},` +
		`"default":{"session":{"max_padding_bytes":30,"max_padding_ratio":0.05,"max_injected":0,"max_pause_total":5,"max_delay":1}}}`).SessionLimits()
	want := shaping.Limits{MaxPaddingBytes: 30, MaxPaddingRatio: 0.1, NoInjection: true, MaxDelay: 2 * time.Second}
	if !ok || l != want {
		t.Errorf("widest %+v, want %+v", l, want)
	}
	if l, ok := mk(`"quic":{"session":{"max_padding_bytes":0}},"default":{"session":{"max_padding_bytes":0,"max_delay":0}}}`).SessionLimits(); !ok || l != (shaping.Limits{NoPadding: true}) {
		t.Errorf("all none %+v", l)
	}
	if l, ok := mk(`"quic":{"session":{"max_padding_bytes":0}},"default":{"session":{"max_padding_bytes":5}}}`).SessionLimits(); !ok || l != (shaping.Limits{MaxPaddingBytes: 5}) {
		t.Errorf("mixed none %+v", l)
	}
	if _, ok := mk(`"quic":{"session":{"max_padding_bytes":1}},"default":"shaped":{}}`).SessionLimits(); ok {
		t.Error("a shaped kind without limits leaves all unlimited")
	}
	if l, ok := mk(`"quic":{"session":{"max_padding_bytes":1}},"default":{"pool":{"count":{"const":1}}}}`).SessionLimits(); !ok || l != (shaping.Limits{MaxPaddingBytes: 1}) {
		t.Errorf("an unshaped kind is skipped: %+v %v", l, ok)
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
