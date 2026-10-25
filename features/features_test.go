// Package features specifies vamos's behavior in Gherkin, in the .feature files beside this file,
// and runs the specifications with godog against the command line, in process,
// as a user would see it: projects in, images and files out.
//
// Each scenario is a subtest of TestFeatures, named by the scenario, or by an outline's example.
// godog's options are flags prefixed godog., such as -godog.tags=@task or -godog.format=progress.
// Scenarios tagged @buildah build images, and are left out with -short or where buildah can't build,
// unless testenv.RequireIntegrationEnv is set, which makes TestFeatures fail instead;
// TestFeatures reports itself skipped when that leaves no scenario, and fails when its tags match none.
// Scenarios share the suite's container store, so they run one at a time.
package features

import (
	"context"
	"flag"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/worker"
	"github.com/cucumber/godog"
)

// opts are godog's options, which flags can set.
var opts = godog.Options{Paths: []string{"."}}

func init() {
	// BindFlags would replace the test binary's usage with godog's, which lists none of the test flags.
	usage := flag.CommandLine.Usage
	godog.BindFlags("godog.", flag.CommandLine, &opts)
	flag.CommandLine.Usage = usage
}

func TestMain(m *testing.M) {
	testenv.Main(m, worker.ChildFromEnv)
}

func TestFeatures(t *testing.T) {
	o := opts
	o.TestingT = t
	o.Strict = true // an undefined or pending step fails its scenario
	if o.Concurrency > 1 {
		// Another scenario's build would leave images in the store that "no image is left behind" can't tell apart.
		t.Fatal("-godog.concurrency must be 1: scenarios share a container store")
	}
	skip, fail := testenv.Buildah()
	if fail != nil {
		t.Fatal(fail)
	} else if skip != "" {
		o.Tags = without(o.Tags, "@buildah")
	}
	var ran int
	suite := godog.TestSuite{
		Name: "vamos",
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
				ran++
				return ctx, nil
			})
			InitializeScenario(sc)
		},
		Options: &o,
	}
	if suite.Run() != 0 {
		t.Fatal("scenarios failed")
	}
	switch {
	case ran == 0 && skip != "":
		// godog would report the scenarios it left out as passed, so report them skipped, as go test would.
		t.Skip(skip)
	case ran == 0:
		t.Fatalf("no scenario matches -godog.tags=%q", o.Tags)
	}
}

// without returns godog's tag expression tags, which commas separate into alternatives,
// with every alternative also excluding tag.
func without(tags, tag string) string {
	if tags == "" {
		return "~" + tag
	}
	alternatives := strings.Split(tags, ",")
	for i, a := range alternatives {
		alternatives[i] = a + " && ~" + tag
	}
	return strings.Join(alternatives, ",")
}

// InitializeScenario gives each scenario a world of its own, and the steps and hooks that act on it.
func InitializeScenario(sc *godog.ScenarioContext) {
	w := &world{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		return ctx, w.start()
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, err error) (context.Context, error) {
		return ctx, w.finish()
	})
	w.register(sc)
}
