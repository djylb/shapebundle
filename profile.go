package shapebundle

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"github.com/zmuxio/zmux-go"
	"github.com/zmuxio/zmux-go/carrier"
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
	policies [256]*zmux.ShapingPolicy
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
		p.policies[i] = c.Policy()
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
	if c := p.policies[sp.Carrier]; c != nil && c.Factory != nil {
		return c.Factory(sp)
	}
	return nil
}

// Factory returns p.Shaper as a zmux.ShaperFactory, for a
// zmux.ShapingPolicy's Factory.
func (p *Profile) Factory() zmux.ShaperFactory { return p.Shaper }

// Budget returns the budget function resolved for k; nil when its entry has
// no budget.
func (p *Profile) Budget(k zmux.CarrierKind) func() shaping.Budget {
	if c := p.policies[k]; c != nil {
		return c.Budget
	}
	return nil
}

// Policy returns the shaping policy of a session that starts on a carrier of
// kind boot, for zmux.Config.Shaping or Conn.SetShaper: each pipe shaped by
// the entry of its own kind, handshakes by Handshake, the direction budget
// that of boot's entry.
func (p *Profile) Policy(boot zmux.CarrierKind) *zmux.ShapingPolicy {
	return &zmux.ShapingPolicy{Factory: p.Shaper, Handshake: p.Handshake(), Budget: p.Budget(boot)}
}

// Handshake returns the handshake shaper of the Profile, for
// zmux.ShapingPolicy.Handshake and the seal configurations: it picks the entry by
// HandshakeView.Kind; an empty kind or one without an entry takes bundle.DefaultKind.
func (p *Profile) Handshake() shaping.HandshakeShaper {
	byName := make(map[string]shaping.HandshakeShaper)
	handshake := func(k int) shaping.HandshakeShaper {
		if c := p.policies[k]; c != nil {
			return c.Handshake
		}
		return nil
	}
	for k := 1; k < len(p.policies); k++ {
		n := bundle.KindName(zmux.CarrierKind(k))
		if h := handshake(k); h != nil && n != bundle.DefaultKind {
			byName[n] = h
		}
	}
	def := handshake(0)
	return func(v *shaping.HandshakeView, u *shaping.UnitPlan) bool {
		h, ok := byName[v.Kind]
		if !ok {
			h = def
		}
		return h != nil && h(v, u)
	}
}

// ConnectionPolicy returns a new physical connection pool policy for k, nil
// when its entry has none (the pool's own logic). Each pool needs its own: a
// policy keeps per-member draws.
func (p *Profile) ConnectionPolicy(k zmux.CarrierKind) carrier.ConnectionPolicy {
	if c := p.compiled[k]; c != nil {
		return c.ConnectionPolicy()
	}
	return nil
}

// SessionGroup resolves the session part of k's pool profile into a session
// controller target over candidates; a target without members when k's entry
// has no pool profile.
func (p *Profile) SessionGroup(k zmux.CarrierKind, id zmux.GroupID, candidates []zmux.TargetCandidate) zmux.GroupTarget {
	if c := p.compiled[k]; c != nil {
		return c.SessionGroup(id, candidates)
	}
	return zmux.GroupTarget{ID: id}
}

// Switch holds the current Profile, so that sessions and connection pools made once
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

// ConnectionPolicy returns a connection pool policy for k that decides by the
// policy of the current Profile, made anew for this pool when the Profile is
// replaced; with no Profile or no policy for k, it leaves the decision to the
// pool.
func (s *Switch) ConnectionPolicy(k zmux.CarrierKind) carrier.ConnectionPolicy {
	return &switchPolicy{s: s, k: k}
}

type switchPolicy struct {
	s     *Switch
	k     zmux.CarrierKind
	mu    sync.Mutex // a pool decides serially; this only guards misuse
	cur   *Profile
	inner carrier.ConnectionPolicy
}

func (sp *switchPolicy) Decide(v *carrier.ConnectionView, d *carrier.ConnectionDecision) error {
	p := sp.s.p.Load()
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if p != sp.cur {
		sp.cur, sp.inner = p, nil
		if p != nil {
			sp.inner = p.ConnectionPolicy(sp.k)
		}
	}
	if sp.inner == nil {
		return nil
	}
	return sp.inner.Decide(v, d)
}
