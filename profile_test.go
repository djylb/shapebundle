package shapebundle

import (
	"sync"
	"testing"
	"time"

	"github.com/zmuxio/zmux-go"
)

func mustParse(t testing.TB, s string) *Bundle {
	t.Helper()
	b, err := Parse([]byte(s), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProfileSingle(t *testing.T) {
	b := mustParse(t, bundleJSON(""))
	p, err := NewProfile(ProfileOptions{}, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		k      zmux.CarrierKind
		shaped bool
	}{{zmux.CarrierWebSocket, true}, {zmux.CarrierQUIC, true}, {zmux.CarrierSplit, false}, {zmux.CarrierHTTP3, true}, {0, true}} {
		if got := p.Shaper(zmux.ShapedPipe{ID: 1, Carrier: c.k}) != nil; got != c.shaped {
			t.Errorf("kind %d: shaped %v", c.k, got)
		}
		if p.Entry(c.k) != b.Entry(c.k) {
			t.Errorf("kind %d: entry differs from the bundle's", c.k)
		}
	}
	if l, ok := p.Limits(zmux.CarrierWebSocket); !ok || l.MaxInjected != 100 {
		t.Errorf("limits %+v %v", l, ok)
	}
	if _, ok := p.Limits(zmux.CarrierQUIC); ok {
		t.Error("quic has no limits")
	}
	pp := p.PoolPolicy(zmux.CarrierWebSocket)
	if pp == nil {
		t.Fatal("no policy")
	}
	var d zmux.PoolDecision
	pp.Decide(&zmux.PoolView{Now: time.Now()}, &d)
	if d.Count != 2 {
		t.Errorf("count %d", d.Count)
	}
	if pp := p.PoolPolicy(zmux.CarrierQUIC); pp != nil {
		t.Errorf("quic pool %v", pp)
	}
	if p.Factory()(zmux.ShapedPipe{Carrier: zmux.CarrierSplit}) != nil {
		t.Error("Factory")
	}
}

func TestProfileOrder(t *testing.T) {
	local := mustParse(t, `{"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":[{"limits":{"max_injected":1}}]}}`)
	remote := mustParse(t, bundleJSON(""))
	byLayer, _ := NewProfile(ProfileOptions{Order: ByLayer}, nil, local, remote)
	byKind, _ := NewProfile(ProfileOptions{Order: ByKind}, local, remote)
	ld, _ := local.Default()
	if byLayer.Entry(zmux.CarrierWebSocket) != ld || byLayer.Entry(0) != ld {
		t.Error("ByLayer: the first layer's default wins")
	}
	if byKind.Entry(zmux.CarrierWebSocket) != remote.Entry(zmux.CarrierWebSocket) || byKind.Entry(zmux.CarrierHTTP3) != ld {
		t.Error("ByKind: a listed kind wins over a default")
	}
	empty, _ := NewProfile(ProfileOptions{})
	if empty.Shaper(zmux.ShapedPipe{}) != nil || empty.Entry(zmux.CarrierQUIC) != nil {
		t.Error("empty profile")
	}
}

func TestSwitch(t *testing.T) {
	var s Switch
	f := s.Factory()
	pp := s.PoolPolicy(zmux.CarrierWebSocket)
	decide := func() int {
		var d zmux.PoolDecision
		pp.Decide(&zmux.PoolView{Now: time.Now()}, &d)
		return d.Count
	}
	if f(zmux.ShapedPipe{Carrier: zmux.CarrierQUIC}) != nil || decide() != 0 {
		t.Error("no profile")
	}
	p, _ := NewProfile(ProfileOptions{}, mustParse(t, bundleJSON("")))
	s.Store(p)
	if f(zmux.ShapedPipe{Carrier: zmux.CarrierQUIC}) == nil || decide() != 2 {
		t.Error("profile")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				f(zmux.ShapedPipe{Carrier: zmux.CarrierWebSocket})
				decide()
			}
		})
	}
	for range 50 {
		s.Store(nil)
		s.Store(p)
	}
	wg.Wait()
	s.Store(nil)
	if decide() != 0 || s.Load() != nil {
		t.Error("cleared")
	}
}
