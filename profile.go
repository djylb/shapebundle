package shapebundle

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"github.com/zmuxio/zmux-go"
	"github.com/zmuxio/zmux-go/shaping"
	"github.com/zmuxio/zmux-go/shaping/machine"
)

// Order is how a Profile resolves a carrier kind over several bundles,
// the first bundle given taking precedence.
type Order uint8

const (
	// ByLayer takes each bundle as a whole: a kind resolves to the first
	// bundle that lists it or DefaultKind, and to that bundle's entry for
	// it, falling back to its DefaultKind entry.
	ByLayer Order = iota
	// ByKind prefers a listed kind over DefaultKind: a kind resolves to
	// the first bundle that lists it, otherwise to the first DefaultKind
	// entry.
	ByKind
)

// ProfileOptions configure a Profile. The zero value is valid.
type ProfileOptions struct {
	// Order resolves kinds over the bundles.
	Order Order
	// NewSource returns the random source of each shaper and pool policy;
	// nil: machine.Options' default. Size bounds come from the
	// ParseOptions.Size each bundle was parsed with.
	NewSource func() rand.Source
}

// Profile is the entry of each carrier kind, resolved over one or more
// bundles, with the shapers of each entry made. It is immutable and safe
// for concurrent use.
type Profile struct {
	o       ProfileOptions
	entries [256]*Entry
	shapers [256]zmux.ShaperFactory
}

// NewProfile resolves every carrier kind over layers, the first taking
// precedence (nil layers are skipped), and makes the shaper factory of
// each definition. Parse already checked every part against its bounds,
// so an error is not expected.
func NewProfile(o ProfileOptions, layers ...*Bundle) (*Profile, error) {
	p := &Profile{o: o}
	def := resolveDefault(layers)
	for i := range p.entries {
		p.entries[i] = def
	}
	for _, k := range kindByName {
		p.entries[k] = resolve(o.Order, layers, k)
	}
	made := map[*Entry]zmux.ShaperFactory{}
	for i, e := range p.entries {
		if e == nil {
			continue
		}
		f, ok := made[e]
		if !ok {
			if e.Definition != nil {
				var err error
				if f, err = e.Definition.Factory(p.options(e)); err != nil {
					return nil, err
				}
			}
			if e.Pool != nil {
				if _, err := e.Pool.Policy(p.options(e)); err != nil {
					return nil, err
				}
			}
			made[e] = f
		}
		p.shapers[i] = f
	}
	return p, nil
}

func (p *Profile) options(e *Entry) machine.Options {
	return machine.Options{NewSource: p.o.NewSource, Limits: e.size}
}

func resolveDefault(layers []*Bundle) *Entry {
	for _, b := range layers {
		if b != nil && b.def != nil {
			return b.def
		}
	}
	return nil
}

func resolve(o Order, layers []*Bundle, k zmux.CarrierKind) *Entry {
	for _, b := range layers {
		if b == nil {
			continue
		}
		if e, ok := b.byKind[k]; ok {
			return e
		}
		if o == ByLayer && b.def != nil {
			return b.def
		}
	}
	return resolveDefault(layers)
}

// Entry returns the entry resolved for k; nil if no bundle has one.
func (p *Profile) Entry(k zmux.CarrierKind) *Entry { return p.entries[k] }

// Shaper returns the Shaper of a pipe by its carrier kind; nil when the
// kind's entry has no definition.
func (p *Profile) Shaper(sp zmux.ShapedPipe) shaping.Shaper {
	if f := p.shapers[sp.Carrier]; f != nil {
		return f(sp)
	}
	return nil
}

// Factory returns p.Shaper as a zmux.ShaperFactory, for zmux.Config.Shaper
// or SetShaper.
func (p *Profile) Factory() zmux.ShaperFactory { return p.Shaper }

// Limits returns the session limits resolved for k, and whether there
// are any (zmux.Config.ShapingLimits).
func (p *Profile) Limits(k zmux.CarrierKind) (shaping.Limits, bool) {
	if e := p.entries[k]; e != nil && e.Session != nil {
		return *e.Session, true
	}
	return shaping.Limits{}, false
}

// SessionLimits returns limits for a session whose pipes may be of any
// kind, such as every session of one listener configuration (zmux.Config
// holds a single ShapingLimits) or a multipath client session mixing
// kinds: field by field the widest over the kinds that are shaped (whose
// entry has a definition), so that no kind's shaping is cut by another's
// limit. Kinds with no entry or no definition add no shaping and are
// skipped; a shaped kind without session limits leaves every field
// unlimited; a No switch is set only if every
// kind sets it. ok is false when the result is no limits (no shaped kind, or one
// without limits).
func (p *Profile) SessionLimits() (l shaping.Limits, ok bool) {
	seen := map[*Entry]bool{}
	var es []*shaping.Limits
	for _, e := range p.entries {
		if e == nil || e.Definition == nil || seen[e] {
			continue // an unshaped kind adds nothing to bound
		}
		seen[e] = true
		if e.Session == nil {
			return shaping.Limits{}, false
		}
		es = append(es, e.Session)
	}
	if len(es) == 0 {
		return shaping.Limits{}, false
	}
	l.NoPadding, l.NoPaddingRatio, l.NoInjection, l.NoPause = true, true, true, true
	// The *Unl flags mark a field some kind leaves unlimited.
	padUnl, ratioUnl, injUnl, pauseUnl, delayUnl := false, false, false, false, false
	for _, x := range es {
		l.NoPadding = l.NoPadding && x.NoPadding
		l.NoPaddingRatio = l.NoPaddingRatio && (x.NoPaddingRatio || x.NoPadding)
		l.NoInjection = l.NoInjection && (x.NoInjection || x.NoPadding)
		l.NoPause = l.NoPause && x.NoPause
		if !x.NoPadding {
			padUnl = padUnl || x.MaxPaddingBytes == 0
			l.MaxPaddingBytes = max(l.MaxPaddingBytes, x.MaxPaddingBytes)
			if !x.NoPaddingRatio {
				ratioUnl = ratioUnl || x.MaxPaddingRatio == 0
				l.MaxPaddingRatio = max(l.MaxPaddingRatio, x.MaxPaddingRatio)
			}
			if !x.NoInjection {
				injUnl = injUnl || x.MaxInjected == 0
				l.MaxInjected = max(l.MaxInjected, x.MaxInjected)
			}
		}
		if !x.NoPause {
			pauseUnl = pauseUnl || x.MaxPauseTotal == 0
			delayUnl = delayUnl || x.MaxDelay == 0
			l.MaxPauseTotal = max(l.MaxPauseTotal, x.MaxPauseTotal)
			l.MaxDelay = max(l.MaxDelay, x.MaxDelay)
		}
	}
	if l.NoPadding {
		l.NoPaddingRatio, l.NoInjection = false, false // implied
	}
	unlimited := func(unl, no bool) bool { return unl && !no }
	if unlimited(padUnl, l.NoPadding) {
		l.MaxPaddingBytes = 0
	}
	if unlimited(ratioUnl, l.NoPaddingRatio) {
		l.MaxPaddingRatio = 0
	}
	if unlimited(injUnl, l.NoInjection) {
		l.MaxInjected = 0
	}
	if unlimited(pauseUnl, l.NoPause) {
		l.MaxPauseTotal = 0
	}
	if unlimited(delayUnl, l.NoPause) {
		l.MaxDelay = 0
	}
	return l, true
}

// PoolPolicy returns a new pool policy for k, nil when its entry has
// none (the pool's own logic). Each pool needs its own: a policy keeps
// per-instance and per-member draws. NewProfile made one of each with the
// same options, so an error is not expected.
func (p *Profile) PoolPolicy(k zmux.CarrierKind) (zmux.PoolPolicy, error) {
	e := p.entries[k]
	if e == nil || e.Pool == nil {
		return nil, nil
	}
	return e.Pool.Policy(p.options(e))
}

// Switch holds the current Profile, so that sessions and pools made once
// follow a Profile replaced later. Its zero value holds none. It is safe
// for concurrent use.
type Switch struct {
	p atomic.Pointer[Profile]
}

// Store makes p the current Profile; nil: none.
func (s *Switch) Store(p *Profile) { s.p.Store(p) }

// Load returns the current Profile; nil if none.
func (s *Switch) Load() *Profile { return s.p.Load() }

// Factory returns a zmux.ShaperFactory that shapes each pipe by the
// Profile current when the pipe's first record is made. A pipe keeps its
// Shaper when the Profile is replaced later: to move the pipes of an open
// session over, call its SetShaper(s.Factory()) again after Store (zmux
// then makes a new Shaper for each pipe at its next record). Session
// limits (zmux.Config.ShapingLimits) are fixed when a session is created,
// so open sessions keep theirs.
func (s *Switch) Factory() zmux.ShaperFactory {
	return func(sp zmux.ShapedPipe) shaping.Shaper {
		if p := s.p.Load(); p != nil {
			return p.Shaper(sp)
		}
		return nil
	}
}

// PoolPolicy returns a pool policy for k that decides by the policy of
// the current Profile, made anew for this pool when the Profile is
// replaced; with no Profile or no policy for k, it leaves the decision to
// the pool.
func (s *Switch) PoolPolicy(k zmux.CarrierKind) zmux.PoolPolicy {
	return &switchPolicy{s: s, k: k}
}

type switchPolicy struct {
	s     *Switch
	k     zmux.CarrierKind
	mu    sync.Mutex // a pool decides serially; this only guards misuse
	cur   *Profile
	inner zmux.PoolPolicy
}

func (sp *switchPolicy) Decide(v *zmux.PoolView, d *zmux.PoolDecision) {
	p := sp.s.p.Load()
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if p != sp.cur {
		sp.cur, sp.inner = p, nil
		if p != nil {
			sp.inner, _ = p.PoolPolicy(sp.k)
		}
	}
	if sp.inner != nil {
		sp.inner.Decide(v, d)
	}
}
