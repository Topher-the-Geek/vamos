package features

import (
	"fmt"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newRemoteWorld starts a scenario of the remote feature, from its background: a coordinator, and a project with a
// layer and a task that add hello.txt, which builds. --coordinator, or $VAMOS_COORDINATOR, names a coordinator that
// builds instead of this process, and from whose stores the command reads what it sends.
func newRemoteWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenTheBuildFileDeclares(`
		glob(name="src", include=["hello.txt"])
		layer(name="app", base="scratch", steps=[add("src", "/data")])
		task(name="copy", base="scratch", steps=[add("src", "/out")], source="/out", include=["hello.txt"])
	`)
	w.GivenACoordinator()
	return w
}

// TestRemote_BuildsOnTheCoordinator specifies that a command given a coordinator builds there, reuses what it built
// there, and sends what it built from there.
func TestRemote_BuildsOnTheCoordinator(t *testing.T) {
	t.Run("A save on a coordinator saves what it built there, which a build there reuses", func(t *testing.T) {
		w := newRemoteWorld(t)
		hello := testenv.NewNonce() // so that what it makes isn't cached already
		w.GivenTheProjectHolds(files{"hello.txt": hello})
		w.WhenISave("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheCoordinatorWasGivenTheBuild()
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": hello})
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.ThenTheCoordinatorWasGivenTheBuild()
	})

	t.Run("An export on a coordinator exports the task's files that it made there", func(t *testing.T) {
		w := newRemoteWorld(t)
		hello := testenv.NewNonce() // so that what it makes isn't cached already
		w.GivenTheProjectHolds(files{"hello.txt": hello})
		w.WhenIExport("copy", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheCoordinatorWasGivenTheBuild()
		w.ThenTheExportHolds(files{"hello.txt": hello})
	})

	t.Run("$VAMOS_COORDINATOR names the coordinator too", func(t *testing.T) {
		w := newRemoteWorld(t)
		hello := testenv.NewNonce() // so that what it makes isn't cached already
		w.GivenTheProjectHolds(files{"hello.txt": hello})
		w.GivenTheCoordinatorHasBuilt("app")
		w.GivenTheEnvironmentSets("VAMOS_COORDINATOR", "<coordinator>")
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused}) // as the coordinator built it
		w.ThenTheCoordinatorWasGivenTheBuild()
	})
}

// TestRemote_BuildsOnWorkers specifies that a coordinator without --local builds on the workers that register with
// it, which push the layers they build to its cache repository, from which a command reads what it sends, and
// pushes, from the client or, with the worker's own settings, from the worker.
func TestRemote_BuildsOnWorkers(t *testing.T) {
	t.Run("A build on a coordinator runs on its worker, and a second build reuses what it made", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistry()
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(`
			glob(name="src", include=["hello.txt"])
			layer(name="app", base="scratch", steps=[add("src", "/data")])
		`)
		hello := testenv.NewNonce()
		w.GivenTheProjectHolds(files{"hello.txt": hello})
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheCoordinatorWasGivenTheBuild()
		w.ThenTheCacheRepositoryHoldsALayer()
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.WhenISave("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": hello})
	})

	t.Run("A pull through a coordinator's pull-through cache pulls from the cache, not the registry it maps",
		func(t *testing.T) {
			w := newBuildingWorld(t)
			w.GivenARegistry()
			w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "base", thisPlatform())
			// No registry answers at upstream.invalid, so only the pull-through cache can serve the pull.
			w.GivenACoordinatorWithAWorker("--pull-through", "upstream.invalid:5000=<registry>")
			w.GivenTheBuildFileDeclares(`
				pull(name="base", image="upstream.invalid:5000/base", tag="v1")
			`)
			w.WhenISave("base", onTheCoordinator("<coordinator>"))
			w.ThenItSucceeds()
			w.ThenTheImageIs("base", thisPlatform())
		})

	for _, example := range []struct {
		name    string
		options []option
	}{
		{name: "A push from the client copies the image from the cache repository",
			options: []option{fromTheClient(), withoutTLSVerification()}},
		{name: "A push from the executor runs on the worker, with its settings"},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildingWorld(t)
			w.GivenARegistry()
			w.GivenACoordinatorWithAWorker()
			w.GivenTheBuildFileDeclares(`
				glob(name="src", include=["hello.txt"])
				layer(name="app", base="scratch", steps=[add("src", "/data")])
			`)
			hello := testenv.NewNonce()
			w.GivenTheProjectHolds(files{"hello.txt": hello})
			w.WhenIPush("app", "<registry>/pushed:v1", append(example.options, onTheCoordinator("<coordinator>"))...)
			w.ThenItSucceeds()
			w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": hello})
		})
	}
}

// TestRemote_InterruptedCoordinatorCancelsItsBuilds specifies that a coordinator serves builds until it is
// interrupted, when it cancels the builds it runs, which end, saying why, and then ends cleanly, promptly.
func TestRemote_InterruptedCoordinatorCancelsItsBuilds(t *testing.T) {
	t.Run("A local coordinator builds in its process", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenALocalCoordinator()
		w.GivenTheBuildFileDeclares(`
			glob(name="src", include=["hello.txt"])
			layer(name="app", base="scratch", steps=[add("src", "/data")])
		`)
		hello := testenv.NewNonce()
		w.GivenTheProjectHolds(files{"hello.txt": hello})
		w.WhenISave("app", onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheCoordinatorWasGivenTheBuild()
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": hello})
	})

	t.Run("A build waiting on a registry ends, cancelled, when its coordinator is interrupted", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistryThatNeverAnswers()
		w.GivenALocalCoordinator()
		w.GivenTheBuildFileDeclares(`
			pull(name="base", image="<registry>/base", tag="v1")
		`)
		w.WhenIInterruptTheCoordinatorWhileIBuild("base")
		w.ThenItFailsWith(`pull "base": context canceled`) // the job's own failure, cancelled, as its last event said
		w.ThenTheCoordinatorWasGivenTheBuild()
		w.ThenTheBuildEndsWithin(5 * time.Second) // with nothing left to release on the coordinator that has gone
		w.ThenTheCoordinatorEndsCleanlyWithin(8 * time.Second)
	})
}

// newRemoteMistakeWorld starts a scenario of the remote feature whose coordinator can't be used, which fails before
// anything is built, so that it needs no buildah: a project with a layer.
func newRemoteMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenTheBuildFileDeclares(`
		layer(name="app", base="scratch")
	`)
	return w
}

// TestRemote_CoordinatorThatCantBeUsedFails specifies that a coordinator that can't be used fails the command,
// saying why: a name that isn't a URL, a coordinator that runs other graph versions, one that can't be reached, and
// a secret, which no coordinator may be sent.
func TestRemote_CoordinatorThatCantBeUsedFails(t *testing.T) {
	for _, example := range []struct {
		name        string
		coordinator string
		error       string
	}{
		{
			name:        "A coordinator that isn't a URL fails",
			coordinator: "coordinator:8080",
			error:       `--coordinator: coordinator "coordinator:8080": want an http or https URL`,
		},
		{
			// Port 9 is discard, which nothing listens on here: the failure is at once, not after a minute of retries.
			name:        "A coordinator that can't be reached fails",
			coordinator: "http://127.0.0.1:9",
			error:       "can't reach the coordinator at http://127.0.0.1:9: ",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newRemoteMistakeWorld(t)
			w.WhenIBuild("app", onTheCoordinator(example.coordinator))
			w.ThenItFailsWith(example.error)
		})
	}

	t.Run("A coordinator that runs other graph versions fails, naming them", func(t *testing.T) {
		w := newRemoteMistakeWorld(t)
		w.GivenACoordinatorThatRunsOnlyGraphVersions(graph.Version + 1000)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"))
		w.ThenItFailsWith(fmt.Sprintf("the coordinator at <coordinator> runs graph versions [%d], not this vamos's, "+
			"%d; use a vamos of its version", graph.Version+1000, graph.Version))
	})

	t.Run("A secret is refused before the coordinator is reached", func(t *testing.T) {
		w := newRemoteMistakeWorld(t)
		w.GivenACoordinatorThatRunsOnlyGraphVersions(graph.Version)
		w.WhenIBuild("app", onTheCoordinator("<coordinator>"), withSecrets("id=token,env=HOME"))
		w.ThenItFailsWith("--secret token can't go to a coordinator, which has no authentication or TLS; " +
			"build in this process, without --coordinator or $VAMOS_COORDINATOR")
		w.ThenTheCoordinatorWasSentNothing()
	})
}
