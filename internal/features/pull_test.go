package features

import (
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
