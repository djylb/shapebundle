package shapebundle_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/zmuxio/zmux-go"
	"github.com/zmuxio/zmux-go/shaping/bundle"

	"github.com/djylb/shapebundle"
	"github.com/djylb/timeshim"
)

// A minimal bundle with only the default kind.
const exampleBundle = `{"schema":2,"generated_at":"2026-01-01T00:00:00Z","next_change":null,"kinds":{` +
	`"default":[{"machines":[{"start":0,"states":[{"record":{"pad":32}}]}]}]}}`

// Fetch a signed bundle from an HTTP endpoint, then build the profile that
// supplies the shaper factory for a zmux session. The endpoint is a local
// test server, so the example needs no network.
func ExampleFetcher() {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(shapebundle.SignatureHeader, hex.EncodeToString(ed25519.Sign(priv, []byte(exampleBundle))))
		_, _ = w.Write([]byte(exampleBundle))
	}))
	defer srv.Close()

	dir, err := os.MkdirTemp("", "shapebundle-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	f, err := shapebundle.NewFetcher(shapebundle.FetcherConfig{
		URL:       srv.URL,
		PublicKey: pub,
		CacheDir:  dir,
		Clock:     timeshim.NewFake(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	b, err := f.Refresh(context.Background())
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(b.GeneratedAt().Format(time.DateOnly))

	p, err := shapebundle.NewProfile(shapebundle.ProfileOptions{}, f.Current())
	if err != nil {
		fmt.Println(err)
		return
	}
	// Pass p.Factory() as the shaper factory of the session configuration.
	fmt.Println(p.Factory() != nil, p.Entry(zmux.CarrierWebSocket) != nil)
	// Output:
	// 2026-01-01
	// true true
}

// Load every bundle of a directory in the order of their names and compose
// them. The first layer takes precedence.
func ExampleLoadDir() {
	dir, err := os.MkdirTemp("", "shapebundle-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err = os.WriteFile(filepath.Join(dir, "10-base.json"), []byte(exampleBundle), 0o600); err != nil {
		fmt.Println(err)
		return
	}

	loaded, err := shapebundle.LoadDir(dir, bundle.ParseOptions{})
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, d := range loaded {
		fmt.Println(d.Name)
	}
	p, err := shapebundle.NewProfile(shapebundle.ProfileOptions{Order: shapebundle.ByKind}, shapebundle.Bundles(loaded)...)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(p.Entry(zmux.CarrierQUIC) != nil)
	// Output:
	// 10-base.json
	// true
}

// A response is accepted only with a valid signature of its body.
func ExampleVerify() {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	body := []byte(exampleBundle)
	sig := hex.EncodeToString(ed25519.Sign(priv, body))

	fmt.Println(shapebundle.Verify(pub, body, sig))
	fmt.Println(shapebundle.Verify(pub, []byte("tampered"), sig))
	// Output:
	// <nil>
	// shapebundle: bad signature
}
