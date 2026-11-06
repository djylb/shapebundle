package shapebundle

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDependencies checks that every package, tests included, depends on
// the standard library, zmux and own libraries alone.
func TestDependencies(t *testing.T) {
	goCmd, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	out, err := exec.Command(goCmd, "list", "-deps", "-test", "-f", "{{.ImportPath}}|{{.Standard}}|{{with .Module}}{{.Path}}{{end}}", "./...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 3 || f[1] == "true" {
			continue
		}
		if f[2] != "github.com/zmuxio/zmux-go" && !strings.HasPrefix(f[2], "github.com/djylb/") {
			t.Errorf("dependency outside the standard library, zmux and own libraries: %s", line)
		}
	}
}
