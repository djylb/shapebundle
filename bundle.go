// Package shapebundle fetches and loads traffic shaping bundles for zmux
// sessions: it verifies their optional Ed25519 signatures, fetches them over
// HTTP into a local cache, loads them from a directory, and composes several
// of them into what zmux takes. Package
// github.com/zmuxio/zmux-go/shaping/bundle parses them.
//
// Nothing here fetches on the path of a connection: a Profile and a Switch
// only read bundles already in memory, and a Fetcher fetches only when
// Refresh is called or while Run runs.
package shapebundle

import (
	"fmt"
	"io"
	"os"

	"github.com/zmuxio/zmux-go/shaping/bundle"
)

// readAll reads r, refusing more than limit bytes (-1: no bound).
func readAll(r io.Reader, limit int, what string) ([]byte, error) {
	if limit < 0 {
		return io.ReadAll(r)
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("shapebundle: %s: more than %d bytes", what, limit)
	}
	return b, nil
}

// ParseFile reads and parses the bundle at path.
func ParseFile(path string, o bundle.ParseOptions) (*bundle.Bundle, error) {
	raw, err := readBounded(path, o.ByteLimit())
	if err != nil {
		return nil, err
	}
	b, err := bundle.Parse(raw, o)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return b, nil
}

// readBounded reads the file at path, refusing one of more than limit bytes.
func readBounded(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readAll(f, limit, path)
}
