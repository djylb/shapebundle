// Package shapebundle loads traffic shaping bundles for zmux sessions.
//
// A bundle is one JSON document holding, per carrier kind, a shaping
// definition, session limits, a pool policy and a handshake prelude (version
// 1 of the zmux shaping configuration format). This package parses and validates
// bundles strictly, verifies their optional Ed25519 signatures, fetches
// them over HTTP into a local cache, loads them from a directory, and
// turns them into a zmux.ShaperFactory, shaping.Limits, zmux.PoolPolicy and
// machine.Prelude values.
//
// Nothing here fetches on the path of a connection: a Profile and a Switch
// only read bundles already in memory, and a Fetcher fetches only when
// Refresh is called or while Run runs.
package shapebundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zmuxio/zmux-go"
	"github.com/zmuxio/zmux-go/shaping"
	"github.com/zmuxio/zmux-go/shaping/machine"
)

// Version is the bundle format version this package reads.
const Version = 1

// DefaultMaxBytes bounds a bundle when ParseOptions.MaxBytes is 0.
const DefaultMaxBytes = 1 << 20

// DefaultKind is the member of a bundle's kinds that applies to a carrier
// kind the bundle does not list, and to pipes of no listed kind (raw TCP
// or TLS, zmux.ShapedPipe.Carrier 0).
const DefaultKind = "default"

// kindNames are the carrier kind names a bundle may list besides
// DefaultKind.
var kindNames = [...]string{"websocket", "streaming", "split", "exchange", "quic", "http3", "webtransport"}

// kindByName maps each of kindNames to its zmux.CarrierKind.
var kindByName = func() map[string]zmux.CarrierKind {
	m := make(map[string]zmux.CarrierKind, len(kindNames))
	for k := 1; k < 256; k++ {
		m[zmux.CarrierKind(k).String()] = zmux.CarrierKind(k)
	}
	out := make(map[string]zmux.CarrierKind, len(kindNames))
	for _, n := range kindNames {
		if k, ok := m[n]; ok {
			out[n] = k
		}
	}
	return out
}()

// KindName returns the name under which a bundle lists k, or DefaultKind
// for a kind a bundle cannot list (0 included).
func KindName(k zmux.CarrierKind) string {
	for n, v := range kindByName {
		if v == k {
			return n
		}
	}
	return DefaultKind
}

// ParseOptions bound a bundle. The zero value is valid.
type ParseOptions struct {
	// MaxBytes bounds the bundle's bytes; 0 is DefaultMaxBytes and a
	// negative value is no bound.
	MaxBytes int
	// Size bounds each definition (machine.Definition.Check), its JSON
	// bytes included (Size.MaxBytes), and the points of each pool
	// or prelude distribution (Size.MaxPoints). Zero values: unbounded. The shapers
	// and pool policies a Profile makes from the bundle keep these bounds.
	Size machine.SizeLimits
}

// maxBytes returns the bound on a bundle's bytes; -1: none.
func (o *ParseOptions) maxBytes() int {
	switch {
	case o.MaxBytes == 0:
		return DefaultMaxBytes
	case o.MaxBytes < 0:
		return -1
	}
	return o.MaxBytes
}

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

// Entry is what a bundle holds for one kind. Each part may be nil: no
// shaping, no session limits, the pool's own logic, no handshake shaping. The
// parts are validated and must not be modified.
type Entry struct {
	Definition *machine.Definition
	Session    *shaping.Limits
	Pool       *machine.PoolSpec
	Prelude    *machine.Prelude

	size machine.SizeLimits // ParseOptions.Size of the bundle
}

// Bundle is a parsed and validated bundle. It is immutable and safe for
// concurrent use.
type Bundle struct {
	raw         []byte
	generatedAt time.Time
	nextChange  time.Time
	byKind      map[zmux.CarrierKind]*Entry
	def         *Entry
	names       []string
}

// GeneratedAt returns the bundle's generated_at.
func (b *Bundle) GeneratedAt() time.Time { return b.generatedAt }

// NextChange returns the bundle's next_change; zero: no planned change.
func (b *Bundle) NextChange() time.Time { return b.nextChange }

// Raw returns a copy of the bytes the bundle was parsed from.
func (b *Bundle) Raw() []byte { return bytes.Clone(b.raw) }

// Kinds returns the names of the kinds the bundle lists, in the order
// they appear.
func (b *Bundle) Kinds() []string { return append([]string(nil), b.names...) }

// Lookup returns the entry the bundle lists for k, and whether it lists
// one, without falling back to DefaultKind.
func (b *Bundle) Lookup(k zmux.CarrierKind) (*Entry, bool) {
	e, ok := b.byKind[k]
	return e, ok
}

// Default returns the bundle's DefaultKind entry, and whether it lists
// one.
func (b *Bundle) Default() (*Entry, bool) { return b.def, b.def != nil }

// Entry returns the entry for k: the one the bundle lists for k, otherwise
// its DefaultKind entry; nil if neither is listed.
func (b *Bundle) Entry(k zmux.CarrierKind) *Entry {
	if e, ok := b.byKind[k]; ok {
		return e
	}
	return b.def
}

// ParseFile reads and parses the bundle at path.
func ParseFile(path string, o ParseOptions) (*Bundle, error) {
	raw, err := readBounded(path, o.maxBytes())
	if err != nil {
		return nil, err
	}
	b, err := Parse(raw, o)
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

// Parse parses and validates a bundle. Parsing is strict at every level:
// unknown and duplicate members are errors, member names and kind names
// are case-sensitive, null is an absent member, version must be the JSON
// integer 1, and the definition, session limits and pool policy of each
// kind are parsed and validated as by machine.Parse, machine.ParseLimits,
// machine.ParsePoolSpec and machine.ParsePrelude. Parse keeps a copy of b.
func Parse(b []byte, o ParseOptions) (*Bundle, error) {
	if limit := o.maxBytes(); limit >= 0 && len(b) > limit {
		return nil, fmt.Errorf("shapebundle: %d bytes, limit %d", len(b), limit)
	}
	if !utf8.Valid(b) {
		return nil, errors.New("shapebundle: not UTF-8")
	}
	bu := &Bundle{raw: bytes.Clone(b), byKind: map[zmux.CarrierKind]*Entry{}}
	dec := json.NewDecoder(bytes.NewReader(bu.raw))
	dec.UseNumber()
	var hasVersion, hasGenerated, hasKinds bool
	err := object(dec, "bundle", false, func(name string) error {
		switch name {
		case "version":
			t, err := dec.Token()
			if err != nil {
				return err
			}
			n, ok := t.(json.Number)
			if t == nil {
				return nil
			}
			if !ok || strings.ContainsAny(string(n), ".eE") {
				return errors.New("version: not an integer")
			}
			if string(n) != "1" {
				return fmt.Errorf("version %s, want %d", n, Version)
			}
			hasVersion = true
		case "generated_at":
			t, err := timeMember(dec, name)
			if err != nil {
				return err
			}
			bu.generatedAt, hasGenerated = t, !t.IsZero()
		case "next_change":
			t, err := timeMember(dec, name)
			if err != nil {
				return err
			}
			bu.nextChange = t
		case "kinds":
			present, err := bu.parseKinds(dec, &o)
			if err != nil {
				return err
			}
			hasKinds = present
		default:
			return fmt.Errorf("unknown field %q", name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("shapebundle: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("shapebundle: data after the bundle")
	}
	switch {
	case !hasVersion:
		return nil, errors.New("shapebundle: missing version")
	case !hasGenerated:
		return nil, errors.New("shapebundle: missing generated_at")
	case !hasKinds:
		return nil, errors.New("shapebundle: missing kinds")
	}
	return bu, nil
}

// object reads a JSON object, calling member for each member name with dec
// positioned at its value; member must consume the value. A null in place
// of the object returns (false, nil) when nullable.
func object(dec *json.Decoder, what string, nullable bool, member func(string) error) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if t == nil && nullable {
		return errNull
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("%s: not an object", what)
	}
	seen := map[string]bool{}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return err
		}
		name := k.(string)
		if seen[name] {
			return fmt.Errorf("%s: duplicate field %q", what, name)
		}
		seen[name] = true
		if err := member(name); err != nil {
			return err
		}
	}
	_, err = dec.Token() // '}'
	return err
}

// errNull reports a null object to its nullable caller.
var errNull = errors.New("null")

// timeMember reads an RFC 3339 string or null (the zero time).
func timeMember(dec *json.Decoder, name string) (time.Time, error) {
	t, err := dec.Token()
	if err != nil || t == nil {
		return time.Time{}, err
	}
	s, ok := t.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("%s: not a string", name)
	}
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", name, err)
	}
	return v, nil
}

// parseKinds reads the kinds member; it reports false for null.
func (b *Bundle) parseKinds(dec *json.Decoder, o *ParseOptions) (bool, error) {
	err := object(dec, "kinds", true, func(name string) error {
		k, known := kindByName[name]
		if !known && name != DefaultKind {
			return fmt.Errorf("kinds: unknown kind %q", name)
		}
		e, err := parseEntry(dec, name, o)
		if err != nil {
			return err
		}
		if e == nil {
			// A null kind is an absent member.
			return nil
		}
		b.names = append(b.names, name)
		if known {
			b.byKind[k] = e
		} else {
			b.def = e
		}
		return nil
	})
	if errors.Is(err, errNull) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(b.names) == 0 {
		return false, errors.New("kinds: empty")
	}
	return true, nil
}

// parseEntry reads one kind's entry; nil for null.
func parseEntry(dec *json.Decoder, kind string, o *ParseOptions) (*Entry, error) {
	e := &Entry{size: o.Size}
	err := object(dec, "kinds."+kind, true, func(name string) error {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if string(raw) == "null" {
			return nil
		}
		where := "kinds." + kind + "." + name
		switch name {
		case "definition":
			if m := o.Size.MaxBytes; m > 0 && len(raw) > m {
				return fmt.Errorf("%s: %d bytes, limit %d", where, len(raw), m)
			}
			d, err := machine.Parse(raw)
			if err == nil {
				err = d.Check(o.Size)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			e.Definition = d
		case "session":
			l, err := machine.ParseLimits(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			e.Session = &l
		case "pool":
			s, err := machine.ParsePoolSpec(raw)
			if err == nil {
				// Policy checks the point bounds; the policy itself is
				// dropped.
				_, err = s.Policy(machine.Options{Limits: o.Size})
			}
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			e.Pool = s
		case "prelude":
			p, err := machine.ParsePrelude(raw)
			if err == nil {
				err = p.Check(o.Size)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			e.Prelude = p
		default:
			return fmt.Errorf("%s: unknown field", where)
		}
		return nil
	})
	if errors.Is(err, errNull) {
		return nil, nil
	}
	return e, err
}
