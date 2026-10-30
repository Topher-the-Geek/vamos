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
