// Package features specifies vamos's behavior as features, rules and scenarios, in Go,
// and runs them against the command line, in process, as a user would see it: projects in, images and files out.
//
// A feature is a file, such as layer_test.go, whose tests are its rules, named TestFeature_Rule,
// each with a doc comment that states one sentence of the BUILD, configuration, cache or CLI reference.
// A rule's scenarios are its subtests, named by their sentences,
// so -run TestTask_ runs a feature, -run TestTask_WithoutOutputIsACheck one rule, and go test -v lists the scenarios.
// A scenario starts from its feature's background, such as newLayerWorld,
// and its steps are methods of world, named as the sentence they stand for, after Given, When or Then:
// Go binds a sentence to its function by name, as Gherkin does by pattern, and go to definition finds it.
// A step's variations are options, such as WhenISave("app", tagged("app:1")), rather than steps of their own.
// A scenario shows its input, in its own text or in a fixture of its feature: steps write only the BUILD text
// they are given, and the pull of busybox that most backgrounds start with.
// What only a running step sees, it reports as observations, which a Then step checks.
// Steps mark themselves helpers, so a failure names the scenario's line.
// Scenarios share the suite's container store, so they run one at a time.
// .agents/kb/testing.md says the rest: what steps check, and how fixtures, tables and probes are written.
package features

import (
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/worker"
)

func TestMain(m *testing.M) {
	testenv.Main(m, worker.ChildFromEnv)
}

// dedent removes the indentation that the non-blank lines of s share, and the blank lines it starts and ends with,
// so that a BUILD file can be indented with the Go that declares it.
func dedent(s string) string {
	blank := func(line string) bool { return strings.TrimSpace(line) == "" }
	lines := strings.Split(s, "\n")
	for len(lines) > 1 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 1 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	var indent string
	found := false
	for _, line := range lines {
		if blank(line) {
			continue
		}
		lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if !found {
			indent, found = lead, true
		}
		for !strings.HasPrefix(lead, indent) {
			indent = indent[:len(indent)-1]
		}
	}
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, indent)
	}
	return strings.Join(lines, "\n") + "\n"
}
