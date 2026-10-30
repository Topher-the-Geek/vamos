package features

import (
	"fmt"
	"testing"

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
