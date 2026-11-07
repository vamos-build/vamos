package features

import (
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newOutputWorld starts a scenario of the output feature, from its background: a project with a layer that adds a
// file.
// A command prints its progress and its result to standard output; one that can't write there stops, and fails
// with the write's failure, as a pipe into a program that has gone, such as head, makes it.
func newOutputWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenTheBuildFileDeclares(`
		glob(name="src", include=["a.txt"])
		layer(name="app", base="scratch", steps=[add("src", "/data")])
	`)
	w.GivenTheProjectHolds(files{"a.txt": "a"})
	return w
}

// TestOutput_ThatCantBeWrittenStopsTheCommand specifies that a command whose standard output can't be written stops,
// and fails with the write's failure.
func TestOutput_ThatCantBeWrittenStopsTheCommand(t *testing.T) {
	t.Run("A build whose progress can't be written stops, and says what it was writing", func(t *testing.T) {
		w := newOutputWorld(t)
		w.GivenBuildahCanBuildHere()
		w.GivenTheProjectHolds(files{"a.txt": testenv.NewNonce()}) // so that the layer isn't cached already
		w.GivenStandardOutputIsClosed()
		w.WhenIBuild("app")
		w.ThenItFailsForTheClosedOutput("writing the build's progress: <closed>")
		w.ThenNoImageIsLeftBehind()
		w.ThenTheCommandLeavesNoContainer()
	})

	t.Run("A graph that can't be written fails", func(t *testing.T) {
		w := newOutputWorld(t)
		w.GivenStandardOutputIsClosed()
		w.WhenIShowTheGraph("app")
		w.ThenItFailsForTheClosedOutput()
	})
}

// The terminal's codes that color a status's first word, each with the code that ends it alone.
const (
	dim, undim     = "\x1b[2m", "\x1b[22m"
	green, ungreen = "\x1b[32m", "\x1b[39m"
)

// TestOutput_StatusesAreColoredInATerminal specifies that a command colors the word its targets' statuses start
// with where standard output is a terminal, NO_COLOR is unset or empty and TERM isn't dumb, or wherever it goes
// with --color=always: cached is dimmed, and building and rebuilding green, so that what runs stands out; the
// rest of a status stays as it is. Elsewhere, or with --color=never, statuses are plain.
func TestOutput_StatusesAreColoredInATerminal(t *testing.T) {
	t.Run("In a terminal, a target that builds is green and one that is cached is dimmed", func(t *testing.T) {
		w := newOutputWorld(t)
		w.GivenBuildahCanBuildHere()
		w.GivenStandardOutputIsATerminal()
		w.GivenTheEnvironmentDoesNotSet("NO_COLOR")
		w.GivenTheEnvironmentSets("TERM", "xterm")
		w.GivenTheProjectHolds(files{"a.txt": testenv.NewNonce()}) // so that the layer isn't cached already
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "app": ` + green + "building" + ungreen + "\n")
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "app": ` + dim + "cached" + undim + "\n")
		w.WhenIBuild("app", rebuilding())
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "app": ` + green + "rebuilding" + ungreen + "\n")
	})

	t.Run("Only the word a status starts with is colored, not why the target builds", func(t *testing.T) {
		w := newOutputWorld(t)
		w.GivenBuildahCanBuildHere()
		w.GivenStoresOfTheScenariosOwn()
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheImagesTheLastCommandCachedAreRemoved() // so that the layer builds again, saying why
		w.WhenIBuild("app", withFlags("--color=always"))
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "app": ` + green + "building" + ungreen +
			", since the cached result can't be used: ")
	})

	t.Run("An empty NO_COLOR leaves statuses colored in a terminal", func(t *testing.T) {
		w := newOutputWorld(t)
		w.GivenBuildahCanBuildHere()
		w.GivenStandardOutputIsATerminal()
		w.GivenTheEnvironmentSets("NO_COLOR", "")
		w.GivenTheEnvironmentSets("TERM", "xterm")
		w.GivenTheProjectHolds(files{"a.txt": testenv.NewNonce()})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "app": ` + green + "building" + ungreen + "\n")
	})

	t.Run("Statuses are plain where standard output isn't a terminal", func(t *testing.T) {
		w := newOutputWorld(t)
		w.GivenBuildahCanBuildHere()
		w.GivenTheEnvironmentDoesNotSet("NO_COLOR")
		w.GivenTheEnvironmentSets("TERM", "xterm")
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenALineOfTheOutputHolds(`layer "app": `)
		w.ThenTheOutputDoesNotInclude("\x1b[")
	})

	for _, example := range []struct {
		name, noColor, term string
		flags               []string
	}{
		{name: "Statuses are plain in a terminal where NO_COLOR is set", noColor: "1", term: "xterm"},
		{name: "Statuses are plain in a terminal whose TERM is dumb", term: "dumb"},
		{name: "Statuses are plain in a terminal with --color=never", term: "xterm", flags: []string{"--color=never"}},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newOutputWorld(t)
			w.GivenBuildahCanBuildHere()
			w.GivenStandardOutputIsATerminal()
			w.GivenTheEnvironmentSets("NO_COLOR", example.noColor)
			w.GivenTheEnvironmentSets("TERM", example.term)
			w.WhenIBuild("app", withFlags(example.flags...))
			w.ThenItSucceeds()
			w.ThenALineOfTheOutputHolds(`layer "app": `)
			w.ThenTheOutputDoesNotInclude("\x1b[")
		})
	}

	for _, example := range []struct{ name, noColor, term string }{
		{name: "--color=always outranks NO_COLOR", noColor: "1", term: "xterm"},
		{name: "--color=always outranks a dumb TERM", term: "dumb"},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newOutputWorld(t)
			w.GivenBuildahCanBuildHere()
			w.GivenTheEnvironmentSets("NO_COLOR", example.noColor)
			w.GivenTheEnvironmentSets("TERM", example.term)
			w.GivenTheProjectHolds(files{"a.txt": testenv.NewNonce()})
			w.WhenIBuild("app", withFlags("--color=always"))
			w.ThenItSucceeds()
			w.ThenTheOutputIncludes(`layer "app": ` + green + "building" + ungreen + "\n")
		})
	}
}
