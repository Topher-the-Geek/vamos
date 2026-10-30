package features

import (
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newPruneWorld starts a scenario of the prune feature, from its background: stores of its own, since prune empties
// them, and a project whose layers and task are built on the image of a registry of its own, base:v1, so that it
// needn't pull busybox. prune removes cached layer images and other build results, while keeping pulled images and
// recorded tag resolutions.
func newPruneWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenStoresOfTheScenariosOwn()
	w.GivenARegistry()
	w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "base", thisPlatform())
	w.GivenTheBuildFileDeclares(`
		pull(name="base", image="<registry>/base", tag="v1")
		glob(name="input", include=["input.txt"])
		layer(name="deps", base="base", steps=[add("input", "/deps")])
		task(name="size", base="base", steps=[add("input", "/work")], source="/work", include=["input.txt"])
		layer(name="app", base="deps", steps=[add("size", "/app")])
	`)
	w.GivenTheProjectHolds(files{"input.txt": testenv.NewNonce() + "\n"})
	return w
}

// TestPrune_RemovesCachedResults specifies that prune removes cached layer images and other build results, so that
// targets run again, while keeping pulled images, and images that other names also name.
func TestPrune_RemovesCachedResults(t *testing.T) {
	t.Run("A prune removes every cached result, and leaves other stores alone", func(t *testing.T) {
		w := newPruneWorld(t)
		w.GivenTheSuitesStoresHoldAResult()
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.ThenTheOutputDoesNotInclude("Removed 0 images")
		w.ThenItLastReports("Removed ")
		w.ThenTheBuildCacheHoldsNothing()
		w.ThenTheSuitesStoresHoldWhatTheyDid()
	})

	t.Run("A prune whose report can't be written still prunes, and fails with the write's failure",
		func(t *testing.T) {
			w := newPruneWorld(t)
			w.WhenIBuild("app")
			w.ThenItSucceeds()
			w.GivenStandardOutputIsClosed()
			w.WhenIPrune()
			w.ThenItFailsForTheClosedOutput()
			w.ThenItTriedToPrintOnly("Removed ") // its report, once it had pruned, so that the write stopped nothing
			w.ThenTheBuildCacheHoldsNothing()
		})

	t.Run("A second prune removes nothing", func(t *testing.T) {
		w := newPruneWorld(t)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.ThenTheOutputIs("Removed 0 images from the container store, and 0 results and 0 files from the build cache\n")
	})

	t.Run("Targets run again after a prune, on the image pulled before, without the registry", func(t *testing.T) {
		w := newPruneWorld(t)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): ran, task("size"): ran, layer("app"): ran})
		w.ThenTheRegistryWasNotAsked()
	})

	t.Run("A cached layer's image that another name names is kept, by that name alone", func(t *testing.T) {
		w := newPruneWorld(t)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenACachedLayerIsAlsoNamed("localhost/vamos-test-kept:v1")
		w.WhenIPrune()
		w.ThenItSucceeds()
		w.ThenTheContainerStoreNamesAnImageOnly("localhost/vamos-test-kept:v1")
	})
}

// TestPrune_WaitsForNoOtherVamos specifies that a prune is refused while another vamos is using the build cache,
// removing nothing, and that a build is refused while a prune runs.
func TestPrune_WaitsForNoOtherVamos(t *testing.T) {
	t.Run("A prune while another vamos builds is refused, and removes nothing", func(t *testing.T) {
		w := newPruneWorld(t)
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenAnotherVamosIsUsingTheBuildCache()
		w.WhenIPrune()
		w.ThenItFailsWith("another vamos is using the build cache")
		w.ThenTheStoresHoldWhatTheyDidBefore()
		w.GivenTheOtherVamosIsDone()
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): reused, layer("app"): reused})
	})

	t.Run("A build while another vamos prunes is refused", func(t *testing.T) {
		w := newPruneWorld(t)
		w.GivenAnotherVamosIsPruningTheBuildCache()
		w.WhenIBuild("app")
		w.ThenItFailsWith("another vamos is pruning the build cache")
		w.ThenNoTargetRan()
		w.GivenTheOtherVamosIsDone()
		w.WhenIBuild("app")
		w.ThenItSucceeds()
	})
}
