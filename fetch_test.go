package shapebundle

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/djylb/timeshim"
)

// server is a bundle endpoint for tests.
type server struct {
	t      *testing.T
	srv    *httptest.Server
	key    ed25519.PrivateKey
	mu     sync.Mutex
	status int
	body   func() string
	badSig bool
	hits   int
	got    []string // request bodies
}

func newServer(t *testing.T, body func() string) *server {
	s := &server{t: t, status: 200, body: body}
	_, s.key, _ = ed25519.GenerateKey(nil)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.hits++
		rb, _ := io.ReadAll(r.Body)
		s.got = append(s.got, string(rb))
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s.status != 200 {
			w.WriteHeader(s.status)
			return
		}
		b := []byte(s.body())
		sig := ed25519.Sign(s.key, b)
		if s.badSig {
			sig[0] ^= 1
		}
		w.Header().Set(SignatureHeader, hex.EncodeToString(sig))
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *server) set(f func(*server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *server) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func (s *server) config(dir string) FetcherConfig {
	return FetcherConfig{
		URL:       s.srv.URL + "/any/path",
		Body:      []byte(`{"kind":"quic"}`),
		Token:     "tok",
		PublicKey: s.key.Public().(ed25519.PublicKey),
		CacheDir:  dir,
		Client:    s.srv.Client(),
	}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestFetcherRefreshAndLoad(t *testing.T) {
	s := newServer(t, func() string { return bundleJSON("") })
	dir := filepath.Join(t.TempDir(), "cache")
	f, err := NewFetcher(s.config(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty cache: %v", err)
	}
	b, err := f.Refresh(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.Current() != b || s.got[0] != `{"kind":"quic"}` {
		t.Error("current or request body")
	}
	path := filepath.Join(dir, DefaultCacheName)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("cache mode %v", st.Mode())
	}
	// A new Fetcher loads the cache without fetching.
	g, _ := NewFetcher(s.config(dir))
	lb, err := g.Load()
	if err != nil || string(lb.Raw()) != string(b.Raw()) || s.count() != 1 {
		t.Fatalf("load: %v", err)
	}

	// Failures keep the current and cached bundle.
	fail := map[string]func(*server){
		"status":    func(s *server) { s.status = 503 },
		"signature": func(s *server) { s.badSig = true },
		"invalid":   func(s *server) { s.body = func() string { return `{"version":2}` } },
		"too big": func(s *server) {
			s.body = func() string { return bundleJSON("") + string(make([]byte, DefaultMaxBytes)) }
		},
	}
	for name, set := range fail {
		var status int
		var badSig bool
		var body func() string
		s.set(func(s *server) { status, badSig, body = s.status, s.badSig, s.body })
		s.set(set)
		if _, err := f.Refresh(ctxT(t)); err == nil {
			t.Errorf("%s: no error", name)
		}
		if f.Current() != b {
			t.Errorf("%s: current replaced", name)
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != string(b.Raw()) {
			t.Errorf("%s: cache replaced", name)
		}
		s.set(func(s *server) { s.status, s.badSig, s.body = status, badSig, body })
	}
	var se *StatusError
	s.set(func(s *server) { s.status = 503 })
	if _, err := f.Refresh(ctxT(t)); !errors.As(err, &se) || se.Code != 503 {
		t.Errorf("status error %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("cache dir holds %d files", len(ents))
	}
}

func TestFetcherToken(t *testing.T) {
	s := newServer(t, func() string { return bundleJSON("") })
	c := s.config("")
	c.Token = "other"
	f, _ := NewFetcher(c)
	var se *StatusError
	if _, err := f.Refresh(ctxT(t)); !errors.As(err, &se) || se.Code != 401 {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := f.Load(); !errors.Is(err, ErrNoBundle) {
		t.Errorf("no cache: %v", err)
	}
}

func TestFetcherConfig(t *testing.T) {
	bad := []FetcherConfig{
		{},
		{URL: "http://x", Body: []byte("{")},
		{URL: "http://x", PublicKey: make([]byte, 5)},
		{URL: "http://x", Jitter: -1},
		{URL: "http://x", RetryMin: time.Hour, RetryMax: time.Minute},
		{URL: "http://x", CacheName: "../x"},
	}
	for i, c := range bad {
		if _, err := NewFetcher(c); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
}

func TestFetcherContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	f, _ := NewFetcher(FetcherConfig{URL: srv.URL, Client: srv.Client(), Timeout: 50 * time.Millisecond})
	start := time.Now()
	if _, err := f.Refresh(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not applied")
	}
}

func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	body := []byte("body")
	sig := hex.EncodeToString(ed25519.Sign(priv, body))
	if err := Verify(pub, body, sig); err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	for name, err := range map[string]error{
		"other key":  Verify(other, body, sig),
		"changed":    Verify(pub, []byte("bodY"), sig),
		"none":       Verify(pub, body, ""),
		"short":      Verify(pub, body, sig[:10]),
		"not hex":    Verify(pub, body, "zz"+sig[2:]),
		"key length": Verify(pub[:5], body, sig),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if k, err := ParsePublicKey(hex.EncodeToString(pub)); err != nil || !k.Equal(pub) {
		t.Error("ParsePublicKey", err)
	}
	if _, err := ParsePublicKey("abcd"); err == nil {
		t.Error("short key accepted")
	}
}

// TestFetcherRun drives Run with a fake clock: a fetch at once, then at
// each next_change, retries with backoff, and no timer left at the end.
func TestFetcherRun(t *testing.T) {
	clock := timeshim.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newServer(t, func() string {
		return bundleJSON(clock.Now().Add(time.Hour).Format(time.RFC3339))
	})
	c := s.config(t.TempDir())
	c.Clock = clock
	c.Jitter = time.Minute
	c.RetryMin, c.RetryMax = 10*time.Second, 40*time.Second
	updates := make(chan *Bundle, 16)
	errs := make(chan error, 16)
	c.OnUpdate = func(b *Bundle) { updates <- b }
	c.OnError = func(err error) { errs <- err }
	f, _ := NewFetcher(c)
	ctx, cancel := context.WithCancel(ctxT(t))
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	next := func() time.Time {
		t.Helper()
		if err := clock.WaitPending(ctxT(t), 1); err != nil {
			t.Fatal(err)
		}
		d := clock.Deadlines()
		if len(d) != 1 {
			t.Fatalf("%d timers", len(d))
		}
		return d[0]
	}
	recv := func(ch <-chan *Bundle) *Bundle {
		t.Helper()
		select {
		case b := <-ch:
			return b
		case <-time.After(10 * time.Second):
			t.Fatal("no update")
			return nil
		}
	}

	// No bundle: fetch within the jitter.
	at := next()
	if at.Sub(clock.Now()) >= time.Minute {
		t.Fatalf("first fetch at %v", at)
	}
	clock.AdvanceTo(at)
	b := recv(updates)

	// The next fetch is at next_change, plus up to the jitter.
	at = next()
	if at.Before(b.NextChange()) || !at.Before(b.NextChange().Add(time.Minute)) {
		t.Fatalf("fetch at %v, next_change %v", at, b.NextChange())
	}

	// Failures retry with backoff: [5, 10) s, then [10, 20) s.
	s.set(func(s *server) { s.status = 500 })
	clock.AdvanceTo(at)
	if err := <-errs; err != nil {
		t.Log(err)
	}
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second} {
		at = next()
		if d := at.Sub(clock.Now()); d < want/2 || d > want {
			t.Fatalf("retry after %v, want up to %v", d, want)
		}
		clock.AdvanceTo(at)
		if err := <-errs; err != nil {
			t.Log(err)
		}
	}
	s.set(func(s *server) { s.status = 200 })
	clock.AdvanceTo(next())
	recv(updates)
	if f.Current() == b {
		t.Error("not replaced")
	}

	// A Load by the caller reschedules Run.
	if _, err := f.Load(); err != nil {
		t.Fatal(err)
	}
	recv(updates)
	next()

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v", err)
	}
	if n := clock.Pending(); n != 0 {
		t.Errorf("%d timers left", n)
	}
}

// TestFetcherRunPassedNextChange checks that a server answering with a
// passed next_change does not make Run fetch in a loop.
func TestFetcherRunPassedNextChange(t *testing.T) {
	clock := timeshim.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newServer(t, func() string { return bundleJSON("2025-01-01T00:00:00Z") })
	c := s.config("")
	c.Clock = clock
	c.RetryMin = time.Minute
	updates := make(chan *Bundle, 4)
	c.OnUpdate = func(b *Bundle) { updates <- b }
	f, _ := NewFetcher(c)
	ctx, cancel := context.WithCancel(ctxT(t))
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	if err := clock.WaitPending(ctx, 1); err != nil {
		t.Fatal(err)
	}
	clock.Advance(0)
	<-updates
	if err := clock.WaitPending(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if d := clock.Deadlines()[0].Sub(clock.Now()); d != time.Minute {
		t.Errorf("next fetch after %v", d)
	}
	cancel()
	if err := <-done; err != nil {
		t.Log(err)
	}

	// MinInterval holds off an earlier next_change.
	c.MinInterval = 2 * time.Hour
	f, _ = NewFetcher(c)
	ctx, cancel = context.WithCancel(ctxT(t))
	defer cancel()
	go func() { done <- f.Run(ctx) }()
	if err := clock.WaitPending(ctx, 1); err != nil {
		t.Fatal(err)
	}
	clock.Advance(0)
	<-updates
	if err := clock.WaitPending(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if d := clock.Deadlines()[0].Sub(clock.Now()); d != 2*time.Hour {
		t.Errorf("next fetch after %v, want MinInterval", d)
	}
	cancel()
	if err := <-done; err != nil {
		t.Log(err)
	}
}
