package shapebundle

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"github.com/zmuxio/zmux-go"
	"github.com/zmuxio/zmux-go/shaping"
	"github.com/zmuxio/zmux-go/shaping/bundle"
	"github.com/zmuxio/zmux-go/shaping/machine"
)

// Order is how a Profile resolves a carrier kind over several bundles,
// the first bundle given taking precedence.
type Order uint8

const (
	// ByLayer takes each bundle as a whole: a kind resolves to the first
	// bundle that lists it or bundle.DefaultKind, and to that bundle's entry for
	// it, falling back to its bundle.DefaultKind entry.
	ByLayer Order = iota
	// ByKind prefers a listed kind over DefaultKind: a kind resolves to
	// the first bundle that lists it, otherwise to the first bundle.DefaultKind
	// entry.
	ByKind
)

// ProfileOptions configure a Profile. The zero value is valid.
type ProfileOptions struct {
	// Order resolves kinds over the bundles.
	Order Order
	// NewSource is machine.Options.NewSource of every candidate.
	NewSource func() rand.Source
}

// Profile is the entry of each carrier kind, resolved over one or more
// bundles, with the shapers of each entry made. It is immutable and safe
// for concurrent use.
type Profile struct {
	o        ProfileOptions
	entries  [256]*bundle.Entry
	compiled [256]*machine.Compiled
	shapers  [256]zmux.ShaperFactory
	handles  [256]shaping.HandshakeShaper
}

// NewProfile resolves every carrier kind over layers, the first taking
// precedence (nil layers are skipped), and compiles the candidate of each entry
// once, with a source of its own.
func NewProfile(o ProfileOptions, layers ...*bundle.Bundle) (*Profile, error) {
	p := &Profile{o: o}
	for i := range p.entries {
		p.entries[i] = resolve(o.Order, layers, zmux.CarrierKind(i))
	}
	made := map[*bundle.Entry]*machine.Compiled{}
	for i, e := range p.entries {
		if e == nil {
			continue
		}
		c, ok := made[e]
		if !ok {
			c = e.Candidate.Compile(machine.Options{NewSource: o.NewSource})
			made[e] = c
		}
		p.compiled[i] = c
		p.shapers[i] = c.Factory()
		p.handles[i] = c.Handshake()
	}
	return p, nil
}

func resolveDefault(layers []*bundle.Bundle) *bundle.Entry {
	for _, b := range layers {
		if b == nil {
			continue
		}
		if e, ok := b.Default(); ok {
			return e
		}
	}
	return nil
}

func resolve(o Order, layers []*bundle.Bundle, k zmux.CarrierKind) *bundle.Entry {
	for _, b := range layers {
		if b == nil {
			continue
		}
		if e, ok := b.Lookup(k); ok {
			return e
		}
		if e, ok := b.Default(); ok && o == ByLayer {
			return e
		}
	}
	return resolveDefault(layers)
}

// Entry returns the entry resolved for k; nil if no bundle has one.
func (p *Profile) Entry(k zmux.CarrierKind) *bundle.Entry { return p.entries[k] }

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

// Budget returns the budget function resolved for k
// (zmux.Config.ShapingBudget); nil when its entry has no budget.
func (p *Profile) Budget(k zmux.CarrierKind) func() shaping.Budget {
	if c := p.compiled[k]; c != nil {
		return c.Budget()
	}
	return nil
}

// Handshake returns the handshake shaper of the Profile, for
// zmux.Config.HandshakeShaper and the seal configurations: it picks the entry by
// HandshakeView.Kind; an empty kind or one without an entry takes bundle.DefaultKind.
func (p *Profile) Handshake() shaping.HandshakeShaper {
	byName := make(map[string]shaping.HandshakeShaper)
	for k := 1; k < len(p.handles); k++ {
		n := bundle.KindName(zmux.CarrierKind(k))
		if h := p.handles[k]; h != nil && n != bundle.DefaultKind {
			byName[n] = h
		}
	}
	def := p.handles[0]
	return func(v *shaping.HandshakeView, u *shaping.UnitPlan) bool {
		h, ok := byName[v.Kind]
		if !ok {
			h = def
		}
		return h != nil && h(v, u)
	}
}

// SessionBudget returns the budget function of a session whose pipes may be of
// any kind, such as every session of one listener configuration or a multipath
// session mixing kinds: field by field the widest over the kinds that are shaped
// (whose entry has a definition), so that no kind is cut by another's budget. A
// shaped kind without a budget leaves it nil, as does no shaped kind; a shaped
// kind without an account or a bound on one wait leaves that part unset.
func (p *Profile) SessionBudget() func() shaping.Budget {
	seen := map[*bundle.Entry]bool{}
	var fs []func() shaping.Budget
	for k, e := range p.entries {
		if e == nil || !e.Candidate.HasMachines() || seen[e] {
			continue
		}
		seen[e] = true
		f := p.compiled[k].Budget()
		if f == nil {
			return nil
		}
		fs = append(fs, f)
	}
	if len(fs) == 0 {
		return nil
	}
	return func() shaping.Budget {
		o := fs[0]()
		for _, f := range fs[1:] {
			x := f()
			switch {
			case o.NoInjection:
				o.Wire = x.Wire
			case !x.NoInjection && (o.Wire == shaping.Account{} || x.Wire == shaping.Account{}):
				o.Wire = shaping.Account{}
			case !x.NoInjection:
				o.Wire = shaping.Account{Initial: max(o.Wire.Initial, x.Wire.Initial), PerMB: max(o.Wire.PerMB, x.Wire.PerMB), PerSec: max(o.Wire.PerSec, x.Wire.PerSec)}
			}
			o.NoInjection = o.NoInjection && x.NoInjection
			if o.MaxDelay <= 0 || x.MaxDelay <= 0 {
				o.MaxDelay = 0
			} else {
				o.MaxDelay = max(o.MaxDelay, x.MaxDelay)
			}
		}
		return o
	}
}

// PoolPolicy returns a new pool policy for k, nil when its entry has
// none (the pool's own logic). Each pool needs its own: a policy keeps
// per-member draws.
func (p *Profile) PoolPolicy(k zmux.CarrierKind) zmux.PoolPolicy {
	if c := p.compiled[k]; c != nil {
		return c.PoolPolicy()
	}
	return nil
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
// session over, call its SetShaper again after Store (zmux then makes a new
// Shaper for each pipe at its next record), with the new Profile's budget; the
// session's account carries over.
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
			sp.inner = p.PoolPolicy(sp.k)
		}
	}
	if sp.inner != nil {
		sp.inner.Decide(v, d)
	}
}
