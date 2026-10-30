package features

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newLoadWorld starts a scenario of the load feature, from its background: a project whose layer "app" adds a file
// to busybox and sets a command. load builds an image and loads it into Docker with the given name.
func newLoadWorld(t *testing.T) *world {
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

// TestLoad_LoadsIntoDocker specifies that load loads the image into Docker with the given name, defaulting its tag
// to :latest, through $DOCKER_HOST, over a Unix socket or plain TCP without $DOCKER_CERT_PATH.
func TestLoad_LoadsIntoDocker(t *testing.T) {
	for _, example := range []struct {
		name    string
		network string
	}{
		{
			name:    "A daemon at a Unix socket loads the image as the name given",
			network: "unix",
		},
		{
			name:    "A daemon at a TCP port loads the image as the name given",
			network: "tcp",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newLoadWorld(t)
			w.GivenADockerDaemonListeningOn(example.network)
			w.WhenILoad("app", "app:dev")
			w.ThenItSucceeds()
			w.ThenTheArchiveNames("docker.io/library/app:dev") // which Docker shows as app:dev
			w.ThenTheImageCommandIs("echo", "hello from vamos")
			w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": "hello, file\n"})
		})
	}

	t.Run("A name without a tag gets :latest", func(t *testing.T) {
		w := newLoadWorld(t)
		w.GivenADockerDaemon()
		w.WhenILoad("app", "app")
		w.ThenItSucceeds()
		w.ThenItLastReports("Image built! app:latest ")
		w.ThenTheArchiveNames("docker.io/library/app:latest")
	})

	t.Run("The image loaded into the host's Docker runs its command", func(t *testing.T) {
		w := newLoadWorld(t)
		w.GivenTheHostsDockerDaemon()
		w.WhenILoad("app", "vamos-test-"+strings.ToLower(testenv.NewNonce())+":latest")
		w.ThenItSucceeds()
		w.ThenDockerRunningTheImagePrints("hello from vamos")
	})
}

// TestLoad_FailureSaysWhy specifies that a load that Docker refuses, or whose daemon can't be reached, fails, saying
// why, once the image is built, for a target other than an index.
func TestLoad_FailureSaysWhy(t *testing.T) {
	t.Run("A daemon that refuses the image fails the load, saying why", func(t *testing.T) {
		w := newLoadWorld(t)
		w.GivenADockerDaemonThatRefusesLoads("no space left on device")
		w.WhenILoad("app", "app:dev")
		w.ThenItFailsWith("loading into Docker: no space left on device")
	})

	t.Run("A daemon that isn't there fails the load, naming where it was looked for, once the image is built",
		func(t *testing.T) {
			w := newLoadWorld(t)
			nonce := testenv.NewNonce()
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				layer(name="echoes", base="busybox", steps=[run(["echo", %q])])
			`, nonce))
			w.GivenNoDockerDaemon()
			w.WhenILoad("echoes", "app:dev")
			w.ThenItFailsWith("loading into Docker", "/nonexistent/docker.sock")
			w.ThenTheTargetsWere(statuses{layer("echoes"): ran})
			w.ThenTheOutputIncludes(nonce)
		})
}

// TestLoad_AnIndexAsksTheDaemonFirst specifies that load of an index asks the Docker daemon its platform before
// anything is built, so a daemon that isn't there fails it at once.
func TestLoad_AnIndexAsksTheDaemonFirst(t *testing.T) {
	t.Run("An index's load without a daemon fails before anything is built", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="scratch", command=["app"])
			index(name="server", layer="app", platforms=["linux/amd64", "linux/arm64"])
		`)
		w.GivenNoDockerDaemon()
		w.WhenILoad("server", "server:dev")
		w.ThenItFailsWith("loading into Docker: ", "unix:///nonexistent/docker.sock")
		w.ThenNoTargetRan()
	})
}

// TestLoad_SendsImagesNotFiles specifies that load accepts pulls, layers and indexes, and not a task, whose image is
// discarded once it runs.
func TestLoad_SendsImagesNotFiles(t *testing.T) {
	t.Run("Loading a task fails before anything is built", func(t *testing.T) {
		w := newWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheBuildFileDeclares(`task(name="make", base="busybox", source="/out", include=["made"])`)
		w.GivenADockerDaemon()
		w.WhenILoad("make", "app:dev")
		w.ThenItFailsWith(`"make" is a task(), whose image is discarded once it runs; ` +
			"name a pull(), layer() or index() to send")
		w.ThenNoTargetRan()
		w.ThenNothingIsLoaded()
	})
}

// TestLoad_AnIndexLoadsTheDaemonsImage specifies that when the target is an index, load selects and builds only the
// image matching the Docker daemon's platform, its variant and all.
func TestLoad_AnIndexLoadsTheDaemonsImage(t *testing.T) {
	t.Run("An amd64 daemon gets the index's amd64 image, and the other platform's target isn't configured",
		func(t *testing.T) {
			w := newBuildingWorld(t)
			nonce := testenv.NewNonce()
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				layer(name="app", base="scratch", command=["app", %q])
				layer(name="broken", base=lambda ctx: fail("configured broken for " + ctx.platform))
				index(name="server", layer={"linux/s390x": "broken", "linux/amd64": "app", "linux/arm64": "app"})
			`, nonce))
			w.GivenADockerDaemon()
			w.WhenILoad("server", "server:dev")
			w.ThenItSucceeds()
			w.ThenTheImageIsFor("linux/amd64")
			w.ThenTheImageCommandIs("app", nonce)
			w.ThenTheOutputDoesNotInclude("broken")
		})

	t.Run("The daemon's image gets the arguments that the index passes on", func(t *testing.T) {
		w := newBuildingWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", params={"greeting": "hi"}, base="scratch",
			      command=lambda ctx: ["app", %q, ctx.greeting])
			index(
			    name="server",
			    params={"greeting": "hi"},
			    layer=dep("app", greeting=arg("greeting")),
			    platforms=["linux/arm64", "linux/amd64"],
			)
		`, nonce))
		w.GivenADockerDaemon()
		w.WhenILoad("server", "server:dev", setting("greeting=hola"))
		w.ThenItSucceeds()
		w.ThenTheImageIsFor("linux/amd64")
		w.ThenTheImageCommandIs("app", nonce, "hola")
	})

	t.Run("An armv7l daemon gets the index's linux/arm/v7 image, listed after linux/arm/v6", func(t *testing.T) {
		w := newBuildingWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="scratch", command=lambda ctx: ["app", %q, ctx.variant])
			index(name="server", layer="app", platforms=["linux/arm/v6", "linux/arm/v7"])
		`, nonce))
		w.GivenADockerDaemonOn("armv7l")
		w.WhenILoad("server", "server:dev")
		w.ThenItSucceeds()
		w.ThenTheImageIsFor("linux/arm/v7")
		w.ThenTheImageCommandIs("app", nonce, "v7")
	})
}
