// Package features specifies Vamos's behavior as features, rules and scenarios, in Go,
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
// A rule that calls parallelRule first runs in parallel with the others that do, and its scenarios with each other,
// each on stores of its own, once the rules that don't have run, one at a time, on the suite's stores:
// those whose steps change what the whole process shares, such as its environment.
// .agents/kb/testing.md says the rest: what steps check, and how fixtures, tables and probes are written.
package features

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/transfer"
	"github.com/vamos-build/vamos/internal/worker"
	"go.podman.io/common/pkg/retry"
)

func TestMain(m *testing.M) {
	// Registry operations retry as often as containers.conf's default, 3 times, but without backing off from a
	// second, which made a scenario whose registry never answers wait 7 seconds, and whatever the host's
	// containers.conf says. Set before Main, since a worker's attempts run in this binary too.
	transfer.RetryLimits = func() (*retry.Options, error) {
		return &retry.Options{MaxRetry: 3, Delay: time.Millisecond}, nil
	}
	// Workers that scenarios start run this binary as their attempts' process, which ChildFromEnv makes one, rather
	// than `vamos attempt`. Set for the whole run, since scenarios that run in parallel can't set the environment;
	// an attempt's process has it set to its store already.
	if os.Getenv(worker.ChildEnv) == "" {
		if err := os.Setenv(worker.ChildEnv, "1"); err != nil {
			panic(err)
		}
	}
	testenv.Main(m, worker.ChildFromEnv)
}

// parallelRules holds the names of the rules that parallelRule marked, and parallelScenarios the scenarios that
// runsInParallel made parallel and that are still running, as tests rather than names, since -count runs a scenario
// again under the same name.
var parallelRules, parallelScenarios sync.Map

// parallelRule runs the rule t in parallel with the other rules that call it, once every rule that doesn't has run,
// and makes each of its scenarios run in parallel with the rule's others as its world starts, on stores of its own.
// A rule calls it first, unless a step of its scenarios changes what the whole process shares, such as the
// environment, as GivenTheEnvironmentSets does with t.Setenv, which panics in a parallel test, the working folder,
// or the umask.
func parallelRule(t *testing.T) {
	t.Parallel()
	parallelRules.Store(t.Name(), true)
}

// runsInParallel reports whether t is a rule that parallelRule marked, or a scenario of one, which it makes run in
// parallel with the rule's other scenarios, once.
func runsInParallel(t *testing.T) bool {
	rule, _, isScenario := strings.Cut(t.Name(), "/")
	if _, ok := parallelRules.Load(rule); !ok {
		return false
	}
	if !isScenario {
		return true
	}
	if _, made := parallelScenarios.LoadOrStore(t, true); !made {
		t.Cleanup(func() { parallelScenarios.Delete(t) })
		t.Parallel()
	}
	return true
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
