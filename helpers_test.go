package shapebundle

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Synthetic fixtures written by hand from the public format.
const (
	defA    = `[{"start":0,"states":[{"record":{"pad":32}}]}]`
	defB    = `[{"start":0,"states":[{"record":{"pad":{"t":[[1,0,64]]}}}]}]`
	session = `{"wire":{"initial":100,"per_mb":100000},"max_delay":null}`
	pool    = `{"count":2,"dial_after":[1000000,5000000],"dial_jitter":100000,` +
		`"retire":[{"metric":"age","above":600000000,"jitter":200000}],"probe_interval":15000000,"probe_jitter":250000}`
)

func bundleJSON(next string) string {
	n := "null"
	if next != "" {
		n = `"` + next + `"`
	}
	return fmt.Sprintf(`{"generated_at":"2026-01-01T00:00:00Z","next_change":%s,"kinds":{`+
		`"websocket":[{"machines":%s,"budget":%s,"pool":%s}],`+
		`"quic":[{"machines":%s}],`+
		`"split":[{}],`+
		`"default":[{"machines":%s,"budget":{"wire":{}}}]}}`, n, defA, session, pool, defB, defB)
}

// TestMain fails the run if a goroutine of this package outlives the
// tests.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		if s := leaked(2 * time.Second); s != "" {
			_, _ = fmt.Fprintln(os.Stderr, "leaked goroutines:\n"+s)
			code = 1
		}
	}
	os.Exit(code)
}

func leaked(wait time.Duration) string {
	end := time.Now().Add(wait)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		var bad []string
		for g := range strings.SplitSeq(string(buf), "\n\n") {
			if strings.Contains(g, "shapebundle.") && !strings.Contains(g, "shapebundle.leaked") && !strings.Contains(g, "shapebundle.TestMain") {
				bad = append(bad, g)
			}
		}
		if len(bad) == 0 || time.Now().After(end) {
			return strings.Join(bad, "\n\n")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
