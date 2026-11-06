package shapebundle

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zmuxio/zmux-go/shaping/bundle"
)

// DirBundle is a bundle read by LoadDir.
type DirBundle struct {
	Name   string // file name in the directory
	Bundle *bundle.Bundle
}

// LoadDir reads every bundle in dir: the regular files whose names end in
// ".json" and do not start with ".", in lexical order of their names. It
// fails on the first file that does not parse. Names starting with "."
// cover the temporary files a Fetcher leaves if it stops mid-write. A missing directory holds
// no bundles. The order is the caller's to use, for example as the layers
// of NewProfile.
func LoadDir(dir string, o bundle.ParseOptions) ([]DirBundle, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []DirBundle
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || !e.Type().IsRegular() {
			continue
		}
		b, err := ParseFile(filepath.Join(dir, name), o)
		if err != nil {
			return nil, err
		}
		out = append(out, DirBundle{Name: name, Bundle: b})
	}
	slices.SortFunc(out, func(a, b DirBundle) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Bundles returns the bundles of d in order.
func Bundles(d []DirBundle) []*bundle.Bundle {
	out := make([]*bundle.Bundle, len(d))
	for i := range d {
		out[i] = d[i].Bundle
	}
	return out
}
