package features

import (
	"testing"
	"time"
)

// newInterruptWorld starts a scenario of the interrupt feature, from its background: a project that pulls busybox,
// which builds. Interrupting vamos, as Ctrl-C does, stops what it is doing promptly, and it reports the interruption.
func newInterruptWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// TestInterrupt_StopsAfterTheRunningStep specifies that a build interrupted while a step runs sends nothing, and
// removes the container that the step ran in before it reports the interruption. The step runs to its end, unseen,
// since buildah can't stop it.
func TestInterrupt_StopsAfterTheRunningStep(t *testing.T) {
	t.Run("A save interrupted while a step runs saves nothing, and leaves no container",
		func(t *testing.T) {
			w := newInterruptWorld(t)
			w.GivenTheBuildFileDeclares(`
				layer(name="app", base="busybox", steps=[run(["sh", "-c", "echo started; sleep 1"])])
			`)
			w.WhenISave("app", interruptedOnceItPrints("started"))
			w.ThenItIsInterrupted()
			w.ThenNothingIsSaved()
			w.ThenTheCommandLeavesNoContainer()
		})
}

// newInterruptedLoadWorld starts a scenario of the interrupt feature whose BUILD file never finishes running,
// which needs no buildah, since nothing is built: a project that pulls busybox, and declares spin, a function that
// runs for days, since Starlark has no while loop, and loops only within functions.
func newInterruptedLoadWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	w.GivenTheBuildFileDeclares(`
		def spin(ctx = None):
		    for i in range(1 << 40):
		        pass
	`)
	return w
}

// TestInterrupt_StopsTheBuildFile specifies that a build interrupted while its BUILD file runs, loading or
// configuring a target, stops promptly, and reports the interruption rather than a mistake in the file.
func TestInterrupt_StopsTheBuildFile(t *testing.T) {
	t.Run("A build interrupted while its BUILD file loads stops", func(t *testing.T) {
		w := newInterruptedLoadWorld(t)
		w.GivenTheBuildFileDeclares(`
			spin()
		`)
		w.WhenIBuild("busybox", interruptedAfter(200*time.Millisecond))
		w.ThenItIsInterrupted()
		w.ThenItStopsWithin(5 * time.Second)
	})

	t.Run("A build interrupted while it configures a target stops", func(t *testing.T) {
		w := newInterruptedLoadWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", steps=spin)
		`)
		w.WhenIBuild("app", interruptedAfter(200*time.Millisecond))
		w.ThenItIsInterrupted()
		w.ThenItStopsWithin(5 * time.Second)
	})
}
