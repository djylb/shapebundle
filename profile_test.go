package shapebundle

import (
	"sync"
	"testing"

	"github.com/zmuxio/zmux-go/shaping/bundle"

	"github.com/zmuxio/zmux-go"
	"github.com/zmuxio/zmux-go/carrier"
	"github.com/zmuxio/zmux-go/shaping"
)

func mustParse(t testing.TB, s string) *bundle.Bundle {
	t.Helper()
	b, err := bundle.Parse([]byte(s), bundle.ParseOptions{})
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
	}{
		{k: zmux.CarrierWebSocket, shaped: true},
		{k: zmux.CarrierQUIC, shaped: true},
		{k: zmux.CarrierSplit, shaped: false},
		{k: zmux.CarrierHTTP3, shaped: true},
		{k: 0, shaped: true},
	} {
		if got := p.Shaper(zmux.ShapedPipe{PipeID: 1, Carrier: c.k}) != nil; got != c.shaped {
			t.Errorf("kind %d: shaped %v", c.k, got)
		}
		if p.Entry(c.k) != b.Entry(c.k) {
			t.Errorf("kind %d: entry differs from the bundle's", c.k)
		}
	}
	if f := p.Budget(zmux.CarrierWebSocket); f == nil || f().Wire == nil || *f().Wire != (shaping.Account{Ceiling: 2000, Initial: 1000, PerMB: 100000}) {
		t.Errorf("websocket budget")
	}
	if p.Budget(zmux.CarrierQUIC) != nil {
		t.Error("quic has no budget")
	}
	pp := p.ConnectionPolicy(zmux.CarrierWebSocket)
	if pp == nil {
		t.Fatal("no policy")
	}
	var d carrier.ConnectionDecision
	if err := pp.Decide(&carrier.ConnectionView{}, &d); err != nil {
		t.Fatal(err)
	}
	if d.Count != 2 {
		t.Errorf("count %d", d.Count)
	}
	if pp := p.ConnectionPolicy(zmux.CarrierQUIC); pp != nil {
		t.Errorf("quic pool %v", pp)
	}
	if p.Factory()(zmux.ShapedPipe{Carrier: zmux.CarrierSplit}) != nil {
		t.Error("Factory")
	}
	if pol := p.Policy(zmux.CarrierWebSocket); pol.Factory == nil || pol.Handshake == nil || pol.Budget == nil {
		t.Errorf("policy %+v", pol)
	}
	if pol := p.Policy(zmux.CarrierQUIC); pol.Budget != nil {
		t.Error("quic policy has a budget")
	}
	// A raw carrier has no entry of its own: it takes the default one.
	if p.Shaper(zmux.ShapedPipe{Carrier: zmux.CarrierKind(8)}) == nil {
		t.Error("a raw carrier is not shaped by the default entry")
	}
}

func TestProfileHandshake(t *testing.T) {
	plan := func(n string) string {
		return `[{"handshakes":{"establish":{"hello":{"pad":` + n + `}}}}]`
	}
	p, err := NewProfile(ProfileOptions{}, mustParse(t, `{"schema":2,"kinds":{"default":`+plan("5")+`,"websocket":`+plan("7")+`}}`))
	if err != nil {
		t.Fatal(err)
	}
	h := p.Handshake()
	for kind, want := range map[string]int{"websocket": 7, "quic": 5, "": 5} {
		var u shaping.UnitPlan
		h(&shaping.HandshakeView{Kind: kind, Handshake: "establish", Message: "hello"}, &u)
		if u.Pad != want {
			t.Errorf("kind %q: pad %d, want %d", kind, u.Pad, want)
		}
	}
}

func TestProfileOrder(t *testing.T) {
	local := mustParse(t, `{"schema":2,"generated_at":"2026-01-01T00:00:00Z","kinds":{"default":[{"budget":{"wire":{"ceiling":1,"initial":1}}}]}}`)
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
	pp := s.ConnectionPolicy(zmux.CarrierWebSocket)
	decide := func() int {
		var d carrier.ConnectionDecision
		if err := pp.Decide(&carrier.ConnectionView{}, &d); err != nil {
			t.Fatal(err)
		}
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
