package features

import (
	"testing"
)

// newPullWorld starts a scenario of the pull feature, from its background: a project that pulls busybox.
func newPullWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

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
			w.WhenISave("base", withFlags("--tls-verify=false"))
			w.ThenItFailsWith(`pull "base": image "127.0.0.1:1/app:latest"`, "connection refused")
			w.ThenTheCommandDoesNotSay("check the name")
			w.ThenNothingIsSaved()
		})
}
