package shapebundle

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/djylb/timeshim"
)

// Defaults of FetcherConfig fields left zero.
const (
	DefaultCacheName = "bundle.json"
	DefaultTimeout   = 30 * time.Second
	DefaultRetryMin  = 30 * time.Second
	DefaultRetryMax  = time.Hour
	DefaultMaxSkew   = time.Hour
)

// ErrNoBundle reports that a Fetcher has no bundle: none is cached and
// none was fetched.
var ErrNoBundle = errors.New("shapebundle: no bundle")

// ErrOlder reports a fetched bundle generated before the current one; it
// is not taken.
var ErrOlder = errors.New("shapebundle: older bundle")

// ErrFuture reports a bundle whose generated_at lies more than MaxSkew
// ahead of the clock; it is not taken.
var ErrFuture = errors.New("shapebundle: bundle from the future")

// StatusError reports a fetch answered with a status other than 200.
type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("shapebundle: fetch: status %d %s", e.Code, http.StatusText(e.Code))
}

// FetcherConfig configures a Fetcher. URL is required.
type FetcherConfig struct {
	// URL is fetched with POST.
	URL string
	// Body is the JSON request body, passed on as it is; nil: empty.
	Body []byte
	// Token, if set, is sent as "Authorization: Bearer <Token>".
	Token string
	// PublicKey, if set, must verify the SignatureHeader of each response.
	// The cache holds only bundles that passed.
	PublicKey ed25519.PublicKey
	// CacheDir holds the cached bundle; "": no cache. It is created 0700
	// if missing.
	CacheDir string
	// CacheName is the cache file's name in CacheDir; "": DefaultCacheName.
	CacheName string
	// Client sends the requests; nil: a client that follows no redirect.
	Client *http.Client
	// Timeout bounds each fetch; 0: DefaultTimeout, negative: none
	// besides the caller's context.
	Timeout time.Duration
	// Parse bounds the bundles, response bodies included.
	Parse ParseOptions
	// Clock times Run; nil: the system clock.
	Clock timeshim.Clock
	// Jitter is the upper bound of a random delay Run adds to each
	// scheduled fetch, so that many fetchers do not fetch at once; 0:
	// none.
	Jitter time.Duration
	// Interval is the time Run waits after a fetch whose bundle has no
	// next_change; 0: Run fetches again only on a failure.
	Interval time.Duration
	// MinInterval is the least time Run leaves between the start of a
	// successful fetch and its next scheduled fetch, whatever next_change
	// says; 0: none. Retries of failed fetches are not bound by it.
	MinInterval time.Duration
	// MaxSkew is how far a bundle's generated_at may lie ahead of Clock's
	// now; a bundle further ahead is refused (ErrFuture), so that one
	// bundle with a wrong clock cannot make every later bundle older. 0:
	// DefaultMaxSkew; negative: no bound.
	MaxSkew time.Duration
	// RetryMin and RetryMax bound the delay before Run retries a failed
	// fetch, doubling from RetryMin to RetryMax with jitter; 0:
	// DefaultRetryMin, DefaultRetryMax. Run also waits RetryMin when a
	// fetched bundle's next_change has already passed.
	RetryMin, RetryMax time.Duration
	// OnUpdate, if set, is called with each bundle that Load or Refresh
	// makes current, in order, before they return. It must not call Load
	// or Refresh.
	OnUpdate func(*Bundle)
	// OnError, if set, is called with each failed fetch of Run.
	OnError func(error)
}

// Fetcher fetches a bundle over HTTP and keeps it in a local cache. It is
// safe for concurrent use.
type Fetcher struct {
	c       FetcherConfig
	clock   timeshim.Clock
	client  *http.Client
	cache   string // cache file path; "": no cache
	mu      sync.Mutex
	last    time.Time // last successful fetch on clock, under mu
	current atomic.Pointer[Bundle]
	changed chan struct{} // wakes Run after Load or Refresh
}

// NewFetcher returns a Fetcher for c.
func NewFetcher(c FetcherConfig) (*Fetcher, error) {
	if c.URL == "" {
		return nil, errors.New("shapebundle: no URL")
	}
	if c.Body != nil && !json.Valid(c.Body) {
		return nil, errors.New("shapebundle: request body is not JSON")
	}
	if c.PublicKey != nil && len(c.PublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("shapebundle: public key of %d bytes, want %d", len(c.PublicKey), ed25519.PublicKeySize)
	}
	if c.Jitter < 0 || c.Interval < 0 || c.MinInterval < 0 || c.RetryMin < 0 || c.RetryMax < 0 {
		return nil, errors.New("shapebundle: negative duration")
	}
	if c.RetryMin == 0 {
		c.RetryMin = DefaultRetryMin
	}
	if c.RetryMax == 0 {
		c.RetryMax = max(DefaultRetryMax, c.RetryMin)
	}
	if c.RetryMax < c.RetryMin {
		return nil, errors.New("shapebundle: RetryMax below RetryMin")
	}
	if c.MaxSkew == 0 {
		c.MaxSkew = DefaultMaxSkew
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.CacheName == "" {
		c.CacheName = DefaultCacheName
	}
	if c.CacheName != filepath.Base(c.CacheName) || c.CacheName == "." || c.CacheName == ".." {
		return nil, fmt.Errorf("shapebundle: cache name %q is not a file name", c.CacheName)
	}
	f := &Fetcher{
		c:       c,
		clock:   timeshim.Or(c.Clock),
		client:  c.Client,
		changed: make(chan struct{}, 1),
	}
	f.c.Body = bytes.Clone(c.Body)
	f.c.PublicKey = bytes.Clone(c.PublicKey)
	if f.client == nil {
		f.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	if c.CacheDir != "" {
		f.cache = filepath.Join(c.CacheDir, c.CacheName)
	}
	return f, nil
}

// Current returns the current bundle; nil before a Load or Refresh
// succeeds.
func (f *Fetcher) Current() *Bundle { return f.current.Load() }

// Load reads the cached bundle and makes it current, under the same rules
// as Refresh: a cached bundle older than the current one (ErrOlder) or too
// far ahead of the clock (ErrFuture) is not taken, and the current one
// stays. The cache is trusted
// local state: its signature is not checked again, so a bundle cached
// under an earlier PublicKey still loads until the next Refresh replaces
// it. It fails with an
// error wrapping fs.ErrNotExist when nothing is cached, and with
// ErrNoBundle when there is no cache.
func (f *Fetcher) Load() (*Bundle, error) {
	if f.cache == "" {
		return nil, ErrNoBundle
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := ParseFile(f.cache, f.c.Parse)
	if err != nil {
		return nil, err
	}
	if err := f.acceptLocked(b, f.current.Load()); err != nil {
		return nil, err
	}
	f.setLocked(b)
	return b, nil
}

// acceptLocked reports whether b may replace base (nil: none): not more
// than MaxSkew ahead of the clock, and not generated before base.
func (f *Fetcher) acceptLocked(b, base *Bundle) error {
	if f.c.MaxSkew > 0 && b.GeneratedAt().After(f.clock.Now().Add(f.c.MaxSkew)) {
		return fmt.Errorf("%w: generated_at %v", ErrFuture, b.GeneratedAt())
	}
	if base != nil && b.GeneratedAt().Before(base.GeneratedAt()) {
		return fmt.Errorf("%w: generated_at %v before %v", ErrOlder, b.GeneratedAt(), base.GeneratedAt())
	}
	return nil
}

// baseLocked returns the bundle a fetched one must not be older than: the
// current one, otherwise a valid cached one that is not from the future.
func (f *Fetcher) baseLocked() *Bundle {
	if cur := f.current.Load(); cur != nil || f.cache == "" {
		return cur
	}
	c, err := ParseFile(f.cache, f.c.Parse)
	if err != nil || f.acceptLocked(c, nil) != nil {
		return nil
	}
	return c
}

// Refresh fetches the bundle, verifies and validates it, writes it to the
// cache and makes it current. A bundle whose generated_at is before the
// current one's, or with none current before the cached one's, fails with
// ErrOlder; one more than MaxSkew ahead of the clock fails with ErrFuture.
// A future bundle is never taken, so it cannot block later ones; a cache
// from the future is ignored as a base and refused by Load, and is
// replaced by the next successful Refresh. On any failure the current and cached
// bundles stay as they were.
func (f *Fetcher) Refresh(ctx context.Context) (*Bundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := f.fetch(ctx)
	if err != nil {
		return nil, err
	}
	b, err := Parse(raw, f.c.Parse)
	if err != nil {
		return nil, err
	}
	if err := f.acceptLocked(b, f.baseLocked()); err != nil {
		return nil, err
	}
	if f.cache != "" {
		if err := writeAtomic(f.c.CacheDir, f.cache, raw); err != nil {
			return nil, err
		}
	}
	f.last = f.clock.Now()
	f.setLocked(b)
	return b, nil
}

func (f *Fetcher) setLocked(b *Bundle) {
	f.current.Store(b)
	if f.c.OnUpdate != nil {
		f.c.OnUpdate(b)
	}
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

// fetch POSTs the request and returns the verified response body.
func (f *Fetcher) fetch(ctx context.Context) ([]byte, error) {
	if f.c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.c.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.c.URL, bytes.NewReader(f.c.Body))
	if err != nil {
		return nil, fmt.Errorf("shapebundle: fetch: %w", err)
	}
	if f.c.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if f.c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.c.Token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shapebundle: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so that the connection may be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, &StatusError{Code: resp.StatusCode}
	}
	raw, err := readAll(resp.Body, f.c.Parse.maxBytes(), "fetch")
	if err != nil {
		return nil, fmt.Errorf("shapebundle: fetch: %w", err)
	}
	if f.c.PublicKey != nil {
		if err := Verify(f.c.PublicKey, raw, resp.Header.Get(SignatureHeader)); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

// writeAtomic replaces path with b: a temporary file in dir, synced, then
// renamed over path; the file is 0600 and dir is created 0700.
func writeAtomic(dir, path string, b []byte) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	t, err := os.CreateTemp(dir, ".bundle-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = t.Close()
			_ = os.Remove(t.Name())
		}
	}()
	if err = t.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if _, err = t.Write(b); err != nil {
		return err
	}
	if err = t.Sync(); err != nil {
		return err
	}
	if err = t.Close(); err != nil {
		return err
	}
	if err = os.Rename(t.Name(), path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename in dir durable where the platform supports it.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// Run fetches in the background until ctx ends, then returns ctx's error.
// It fetches at once when there is no current bundle, otherwise at the
// current bundle's next_change (at once if it has passed), or Interval
// after the last fetch when the bundle has none; each fetch is delayed by
// up to Jitter. A failed fetch keeps the current bundle and is retried
// with backoff. A Load or Refresh by the caller reschedules Run. Run
// starts no goroutine of its own besides the clock's timers, which it
// stops before it returns.
func (f *Fetcher) Run(ctx context.Context) error {
	failures := 0
	for {
		d, ok := f.delay(failures)
		var t timeshim.Timer
		// A channel per round: a timer stopped late cannot wake a later
		// round early.
		wake := make(chan struct{}, 1)
		if ok {
			t = f.clock.AfterFunc(d, func() {
				select {
				case wake <- struct{}{}:
				default:
				}
			})
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return ctx.Err()
		case <-f.changed:
			if t != nil {
				t.Stop()
			}
			failures = 0
			continue
		case <-wake:
		}
		if _, err := f.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures++
			if f.c.OnError != nil {
				f.c.OnError(err)
			}
			continue
		}
		failures = 0
		// Refresh signalled changed; take it so the loop does not
		// reschedule twice.
		select {
		case <-f.changed:
		default:
		}
	}
}

// delay returns the wait before Run's next fetch, and false for none.
func (f *Fetcher) delay(failures int) (time.Duration, bool) {
	if failures > 0 {
		d := f.c.RetryMin
		for i := 1; i < failures && d < f.c.RetryMax; i++ {
			d *= 2
		}
		d = min(d, f.c.RetryMax)
		// Between half and all of it.
		return d/2 + randDuration(d-d/2), true
	}
	b := f.current.Load()
	if b == nil {
		return randDuration(f.c.Jitter), true
	}
	f.mu.Lock()
	last := f.last
	f.mu.Unlock()
	now := f.clock.Now()
	var d time.Duration
	switch next := b.NextChange(); {
	case !next.IsZero() && !last.IsZero() && !next.After(last):
		// The server still answers with a passed next_change.
		d = f.c.RetryMin
	case !next.IsZero():
		d = max(next.Sub(now), 0)
	case f.c.Interval > 0 && !last.IsZero():
		d = max(last.Add(f.c.Interval).Sub(now), 0)
	case f.c.Interval > 0:
		// A cached bundle: never fetched by this Fetcher.
		d = 0
	default:
		return 0, false
	}
	if !last.IsZero() && f.c.MinInterval > 0 {
		d = max(d, last.Add(f.c.MinInterval).Sub(now))
	}
	return d + randDuration(f.c.Jitter), true
}

// randDuration returns a uniform random duration in [0, d).
func randDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d)
}
