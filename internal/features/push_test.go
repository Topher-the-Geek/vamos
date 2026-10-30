package features

import (
	"testing"
)

// newPushWorld starts a scenario of the push feature, from its background: a project whose layer "app" adds a file
// to busybox and sets a command, and a registry of the scenario's own, which <registry> stands for.
// push publishes an image, or a whole index with its images, to a repository.
func newPushWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	w.GivenTheBuildFileDeclares(`
		glob(name="hello", include=["hello.txt"])
		layer(name="app", base="busybox", steps=[add("hello", "/data")], command=["echo", "hello from vamos"])
	`)
	w.GivenTheProjectHolds(files{"hello.txt": "hello, file\n"})
	return w
}

// newPushMistakeWorld starts a scenario of the push feature whose push fails before anything is built,
// so that it needs no buildah. Its project declares the layer "app" on busybox.
func newPushMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", command=["echo", "hello from vamos"])`)
	return w
}

// TestPush_PublishesToTheRepository specifies that push publishes an image to the destination repository,
// from the executor by default, or from the client with --push-from=client,
// and that an image name without a tag gets :latest.
func TestPush_PublishesToTheRepository(t *testing.T) {
	for _, example := range []struct {
		name    string
		options []option
	}{
		{
			name: "The executor pushes the image, by default",
		},
		{
			name:    "The client pushes the image with --push-from=client",
			options: []option{fromTheClient(), withoutTLSVerification()},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPushWorld(t)
			w.GivenARegistry()
			w.WhenIPush("app", "<registry>/team/app:v1", example.options...)
			w.ThenItSucceeds()
			w.ThenTheImageCommandIs("echo", "hello from vamos")
			w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": "hello, file\n"})
		})
	}

	t.Run("An image name without a tag gets :latest", func(t *testing.T) {
		w := newPushWorld(t)
		w.GivenARegistry()
		w.WhenIPush("app", "<registry>/app")
		w.ThenItSucceeds()
		w.ThenItLastReports("Image built! <registry>/app:latest ")
	})
}

// TestPush_EachSideUsesItsOwnSettings specifies that the executor pushes with the registry settings and credentials
// of its own environment, registries.conf and REGISTRY_AUTH_FILE, and that only the client honours the command line's
// --tls-verify, which given alone means true, and otherwise defers to registries.conf.
func TestPush_EachSideUsesItsOwnSettings(t *testing.T) {
	t.Run("The executor ignores --tls-verify=false, pushing over HTTPS where registries.conf says to",
		func(t *testing.T) {
			w := newPushWorld(t)
			w.GivenARegistryThatRegistriesConfDoesNotMarkInsecure()
			w.WhenIPush("app", "<registry>/app", withoutTLSVerification())
			w.ThenItFailsWith("pushing to <registry>/app:latest", "HTTP response to HTTPS client")
			w.ThenNothingIsPushed()
		})

	t.Run("The executor pushes over HTTPS where registries.conf says to", func(t *testing.T) {
		w := newPushWorld(t)
		w.GivenARegistryThatRegistriesConfDoesNotMarkInsecure()
		w.WhenIPush("app", "<registry>/app")
		w.ThenItFailsWith("pushing to <registry>/app:latest", "HTTP response to HTTPS client")
		w.ThenNothingIsPushed()
	})

	t.Run("The client honours --tls-verify=false", func(t *testing.T) {
		w := newPushWorld(t)
		w.GivenARegistryThatRegistriesConfDoesNotMarkInsecure()
		w.WhenIPush("app", "<registry>/app", fromTheClient(), withoutTLSVerification())
		w.ThenItSucceeds()
	})

	t.Run("The client pushes over HTTPS without --tls-verify, where registries.conf says to", func(t *testing.T) {
		w := newPushWorld(t)
		w.GivenARegistryThatRegistriesConfDoesNotMarkInsecure()
		w.WhenIPush("app", "<registry>/app", fromTheClient())
		w.ThenItFailsWith("pushing to <registry>/app:latest", "HTTP response to HTTPS client")
		w.ThenNothingIsPushed()
	})

	t.Run("The client pushes over plain HTTP without --tls-verify, where registries.conf says to", func(t *testing.T) {
		w := newPushWorld(t)
		w.GivenARegistry()
		w.WhenIPush("app", "<registry>/app", fromTheClient())
		w.ThenItSucceeds()
	})

	t.Run("The client given --tls-verify alone pushes over HTTPS, whatever registries.conf says", func(t *testing.T) {
		w := newPushWorld(t)
		w.GivenARegistry()
		w.WhenIPush("app", "<registry>/app", fromTheClient(), withTLSVerification())
		w.ThenItFailsWith("pushing to <registry>/app:latest", "HTTP response to HTTPS client")
		w.ThenNothingIsPushed()
	})

	t.Run("The executor logs in as REGISTRY_AUTH_FILE says", func(t *testing.T) {
		w := newPushWorld(t)
		w.GivenARegistryThatNeedsALogin("vamos", "secret")
		w.GivenTheAuthFileLogsIn("vamos", "secret")
		w.WhenIPush("app", "<registry>/app")
		w.ThenItSucceeds()
	})

	t.Run("The executor without a login to a registry that needs one fails", func(t *testing.T) {
		w := newPushWorld(t)
		w.GivenARegistryThatNeedsALogin("vamos", "secret")
		w.WhenIPush("app", "<registry>/app")
		w.ThenItFailsWith("pushing to <registry>/app:latest", "authentication required")
		w.ThenNothingIsPushed()
	})

	t.Run("A REGISTRY_AUTH_FILE that isn't there fails, naming it, before anything is built", func(t *testing.T) {
		w := newPushMistakeWorld(t)
		w.GivenARegistry()
		w.GivenTheEnvironmentSets("REGISTRY_AUTH_FILE", "/nonexistent/auth.json")
		w.WhenIPush("app", "<registry>/app")
		w.ThenItFailsWith("REGISTRY_AUTH_FILE", "/nonexistent/auth.json", "no such file")
		w.ThenNoTargetRan()
		w.ThenNothingIsPushed()
	})
}

// TestPush_FailureSaysWhy specifies that a push to a registry that can't be reached fails, saying why.
func TestPush_FailureSaysWhy(t *testing.T) {
	for _, example := range []struct {
		name    string
		options []option
	}{
		{
			name: "A registry that isn't running fails the executor, saying the connection was refused",
		},
		{
			name:    "A registry that isn't running fails the client, saying the connection was refused",
			options: []option{fromTheClient(), withoutTLSVerification()},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPushWorld(t)
			w.WhenIPush("app", "127.0.0.1:1/app", example.options...)
			w.ThenItFailsWith("pushing to 127.0.0.1:1/app:latest", "connection refused")
		})
	}
}
