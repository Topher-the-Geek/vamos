package features

import (
	"fmt"
	"strings"
	"testing"
)

// newPullWorld starts a scenario of the pull feature, from its background: a project that pulls busybox.
func newPullWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newPullMistakeWorld starts a scenario of the pull feature whose pull is declared wrongly,
// which loading catches before anything runs, so that it needs no buildah.
// Its project pulls busybox.
func newPullMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newPullTagWorld starts a scenario of the pull feature whose pull is of a tag in a registry of the scenario's own,
// which <registry> stands for, and which holds the index "first" at base:v1, of an image for this machine's platform
// and another. Its project pulls base:v1 as "base" and builds the layer "app" on it.
func newPullTagWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenARegistry()
	w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "first", thisPlatform(), anotherPlatform())
	w.GivenTheBuildFileDeclares(appOnTheTag)
	return w
}

// appOnTheTag declares the pull "base" of the tag v1 of base in the scenario's registry, and the layer "app" on it.
const appOnTheTag = `
	pull(name="base", image="<registry>/base", tag="v1")
	layer(name="app", base="base")
`

// TestPull_FetchesItsImage specifies that a pull fetches the image for its configured platform.
func TestPull_FetchesItsImage(t *testing.T) {
	t.Run("A pull fetches the image for this machine's platform by default", func(t *testing.T) {
		w := newPullWorld(t)
		w.WhenISave("busybox")
		w.ThenItSucceeds()
		w.ThenTheImageIsForThisMachine()
	})

	t.Run("A pull fetches the image for the platform the command gives", func(t *testing.T) {
		w := newPullWorld(t)
		platform := anotherPlatform()
		w.WhenISave("busybox", forPlatform(platform))
		w.ThenItSucceeds()
		w.ThenTheImageIsFor(platform)
	})
}

// TestPull_FailureSaysWhy specifies that a pull whose image its registry doesn't have, or won't give, fails,
// suggesting to check the name, and to log in if the image is private, and that one whose registry can't be reached
// fails, saying why.
func TestPull_FailureSaysWhy(t *testing.T) {
	t.Run("An image the registry won't give fails, suggesting to check the name", func(t *testing.T) {
		// Docker Hub won't give an image it doesn't have, as it won't give a private one.
		w := newPullWorld(t)
		w.GivenTheBuildFileDeclares(`
			pull(name="base", image="vamos-no-such-image")
			layer(name="app", base="base")
		`)
		w.WhenISave("app")
		w.ThenItFailsWith(`pull "base": image "docker.io/library/vamos-no-such-image:latest"`,
			"check the name, and log in if the image is private")
		w.ThenNothingIsSaved()
	})

	t.Run("A registry that can't be reached fails, saying so rather than suggesting the name is wrong",
		func(t *testing.T) {
			w := newPullWorld(t)
			w.GivenTheBuildFileDeclares(`pull(name="base", image="127.0.0.1:1/app")`)
			w.WhenISave("base", withoutTLSVerification())
			w.ThenItFailsWith(`pull "base": image "127.0.0.1:1/app:latest"`, "connection refused")
			w.ThenTheCommandDoesNotSay("check the name")
			w.ThenNothingIsSaved()
		})
}

// TestPull_ImageIsARepositoryWithoutATagOrDigest specifies that a pull's image is a repository name,
// without a tag or digest: an image's tag belongs in tag, rather than inside image.
func TestPull_ImageIsARepositoryWithoutATagOrDigest(t *testing.T) {
	for _, example := range []struct {
		name  string
		pull  string // on line 2, after the pull of busybox
		error string
	}{
		{
			name:  "A pull without an image fails",
			pull:  `pull(name="app")`,
			error: "/project/BUILD:2:5: pull: missing argument for image",
		},
		{
			name:  "A pull with an empty image fails",
			pull:  `pull(name="app", image="")`,
			error: "/project/BUILD:2:5: pull: image must not be empty",
		},
		{
			name:  "A pull with an image that isn't a repository name fails",
			pull:  `pull(name="app", image="Busy Box")`,
			error: `/project/BUILD:2:5: pull: invalid image "Busy Box"`,
		},
		{
			name:  "A pull with a tag in its image fails, saying to give it as tag",
			pull:  `pull(name="app", image="busybox:1.36")`,
			error: `/project/BUILD:2:5: pull: image "busybox:1.36" must not include a tag; use tag="1.36"`,
		},
		{
			name: "A pull with a digest in its image fails",
			pull: `pull(name="app", image="busybox@sha256:` + strings.Repeat("0", 64) + `")`,
			error: `/project/BUILD:2:5: pull: image "busybox@sha256:` + strings.Repeat("0", 64) +
				`" must not include a digest`,
		},
		{
			name:  "A pull with a tag that isn't one fails",
			pull:  `pull(name="app", image="busybox", tag="1 36")`,
			error: `/project/BUILD:2:5: pull: invalid tag "1 36"`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPullMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.pull)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestPull_TakesNoParameters specifies that pull targets have no user parameters; only their platform configures them.
func TestPull_TakesNoParameters(t *testing.T) {
	t.Run("A pull that declares parameters fails", func(t *testing.T) {
		w := newPullMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`pull(name="app", image="busybox", params={"mode": "release"})`)
		w.WhenISave("app")
		w.ThenItFailsWith("/project/BUILD:2:5: pull: a pull takes no parameters; only its platform configures it")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestPull_ResolvedTagsAreReused specifies that a pull of a tag that names an index selects the matching image,
// and that a tag resolved before is reused, for every platform, until it is explicitly rebuilt.
func TestPull_ResolvedTagsAreReused(t *testing.T) {
	t.Run("A pull selects its index's image for the platform", func(t *testing.T) {
		w := newPullTagWorld(t)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageIsForThisMachine()
		w.ThenTheImageIsBuiltOn("first", thisPlatform())
	})

	t.Run("A tag that moves keeps naming what it named, without asking the registry", func(t *testing.T) {
		w := newPullTagWorld(t)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "second", thisPlatform(), anotherPlatform())
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheRegistryWasNotAsked()
		w.ThenTheImageIsBuiltOn("first", thisPlatform())
	})

	t.Run("A platform not built for before gets the image of the index the tag named then", func(t *testing.T) {
		w := newPullTagWorld(t)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "second", thisPlatform(), anotherPlatform())
		w.WhenISave("app", forPlatform(anotherPlatform()))
		w.ThenItSucceeds()
		w.ThenTheImageIsFor(anotherPlatform())
		w.ThenTheImageIsBuiltOn("first", anotherPlatform())
	})

	t.Run("--rebuild resolves the tag again, for every platform", func(t *testing.T) {
		w := newPullTagWorld(t)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "second", thisPlatform(), anotherPlatform())
		w.WhenIBuild("base", rebuilding())
		w.ThenItSucceeds()
		w.ThenTheOutputIs(`pull "base": pulling again` + "\n")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageIsBuiltOn("second", thisPlatform())
		w.WhenISave("app", forPlatform(anotherPlatform()))
		w.ThenItSucceeds()
		w.ThenTheImageIsBuiltOn("second", anotherPlatform())
	})

	t.Run("After a prune, builds use the images that the tag last resolved to, without the registry",
		func(t *testing.T) {
			w := newPullTagWorld(t)
			w.GivenStoresOfTheScenariosOwn() // since it prunes them
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "second", thisPlatform(), anotherPlatform())
			w.WhenIBuild("base", rebuilding())
			w.ThenItSucceeds()
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.WhenISave("app", forPlatform(anotherPlatform()))
			w.ThenItSucceeds()
			w.WhenIPrune()
			w.ThenItSucceeds()
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheRegistryWasNotAsked()
			w.ThenTheImageIsBuiltOn("second", thisPlatform())
			w.GivenTheRegistryStops()
			w.WhenISave("app", forPlatform(anotherPlatform()))
			w.ThenItSucceeds()
			w.ThenTheImageIsBuiltOn("second", anotherPlatform())
			w.WhenISave("base")
			w.ThenItSucceeds()
			w.ThenTheImageIs("second", thisPlatform())
		})
}

// TestPull_ThePlatformMustMatch specifies that a pull of a tag that names an index selects the matching image,
// which the index must have, and that a single-platform image must match the configured platform.
func TestPull_ThePlatformMustMatch(t *testing.T) {
	t.Run("An index without the platform's image fails, naming those it has", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "index", "linux/s390x", "linux/ppc64le")
		w.GivenTheBuildFileDeclares(appOnTheTag)
		w.WhenIBuild("app")
		w.ThenItFailsWith(`pull "base": image "<registry>/base:v1"`,
			fmt.Sprintf(`its index has no image for %s; it has "linux/ppc64le", "linux/s390x"`, thisPlatform()))
	})

	t.Run("An image for another platform fails, saying its tag names it alone", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:v1", "single", "linux/s390x")
		w.GivenTheBuildFileDeclares(appOnTheTag)
		w.WhenIBuild("app")
		w.ThenItFailsWith(`pull "base": image "<registry>/base:v1"`,
			fmt.Sprintf("is for linux/s390x, not %s, and its tag names that image alone, "+
				"not an index of images for several platforms", thisPlatform()))
	})
}
