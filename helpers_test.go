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
	defA    = `{"version":1,"machines":[{"states":[{"record":{"pad":{"const":32}}}]}]}`
	defB    = `{"version":1,"machines":[{"states":[{"record":{"pad":{"range":[0,64]}}}]}]}`
	session = `{"max_padding_ratio":0.1,"max_injected":100,"max_delay":null}`
	pool    = `{"count":{"const":2},"dial_after":[{"const":1},{"const":5}],"dial_jitter":0.1,` +
		`"retire":[{"metric":"age","above":{"const":600},"jitter":0.2}],"probe_interval":{"const":15},"probe_jitter":0.25}`
)

func bundleJSON(next string) string {
	n := "null"
	if next != "" {
		n = `"` + next + `"`
	}
	return fmt.Sprintf(`{"version":1,"generated_at":"2026-01-01T00:00:00Z","next_change":%s,"kinds":{`+
		`"websocket":{"definition":%s,"session":%s,"pool":%s},`+
		`"quic":{"definition":%s},`+
		`"split":{},`+
		`"default":{"definition":%s,"session":{"max_padding_bytes":0}}}}`, n, defA, session, pool, defB, defB)
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
