package features

import (
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newLayerWorld starts a scenario of the layer feature, from its background.
// A layer takes an image and files in and gives an image out.
// It starts from its base's image, applies its steps in order,
// and records the runtime configuration that the image runs with.
// Its project pulls busybox.
func newLayerWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// TestLayer_ImageIsBasesWithChanges specifies that a layer's image is its base's, with the layer's changes.
func TestLayer_ImageIsBasesWithChanges(t *testing.T) {
	t.Run("A layer keeps its base's files and adds its own", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="deps_files", include=["deps.txt"])
			layer(
			    name="deps",
			    base="busybox",
			    steps=[add("deps_files", "/deps"), run(["sh", "-c", "echo installed > /deps/installed.txt"])],
			)
			glob(name="app_files", include=["app.txt"])
			layer(name="app", base="deps", steps=[add("app_files", "/app"), run(["cat", "/deps/installed.txt"])])
		`)
		w.GivenTheProjectHolds(files{
			"deps.txt": "a dependency\n",
			"app.txt":  "the app\n",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes("installed")
		w.ThenTheImageHolds(under("/deps"), files{
			"/deps/deps.txt":      "a dependency\n",
			"/deps/installed.txt": "installed\n",
		})
		w.ThenTheImageHolds(under("/app"), files{"/app/app.txt": "the app\n"})
		w.ThenNoImageIsLeftBehind()
	})

	t.Run("A layer adds tasks' outputs and project files into one directory", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="input", include=["input.txt"])
			task(
			    name="shout",
			    base="busybox",
			    steps=[run(["sh", "-c", """
			        mkdir -p /out/sub
			        echo HELLO > /out/upper.txt
			        echo nested > /out/sub/nested.txt
			        """])],
			    source="/out",
			    include=["upper.txt", "sub"],
			)
			task(
			    name="count",
			    base="busybox",
			    steps=[run(["sh", "-c", "mkdir /out && echo 6 > /out/size.txt"])],
			    source="/out",
			    include=["size.txt"],
			)
			layer(
			    name="app",
			    base="busybox",
			    steps=[add("shout", "/app"), add("count", "/app"), add("input", "/app")],
			)
		`)
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{
			"/app/upper.txt":      "HELLO\n",
			"/app/sub/nested.txt": "nested\n",
			"/app/size.txt":       "6\n",
			"/app/input.txt":      "hello\n",
		})
		w.ThenNoImageIsLeftBehind()
	})

	t.Run("A layer on scratch holds only what it adds", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="hello", include=["hello.txt"])
			layer(name="app", base="scratch", steps=[add("hello", "/data")])
		`)
		w.GivenTheProjectHolds(files{"hello.txt": "hello, file\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(everywhere, files{"/data/hello.txt": "hello, file\n"})
	})

	t.Run("A failing step fails the layer and leaves nothing behind", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="deps", base="busybox", steps=[run(["touch", "/installed"])])
			layer(name="app", base="deps", steps=[run(["false"])])
		`)
		w.WhenISave("app")
		w.ThenItFailsWith(`layer "app": run(["false"]) failed`)
		w.ThenNothingIsSaved()
		w.ThenNoImageIsLeftBehind()
	})

	t.Run("workdir() where a file stands fails the layer", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[workdir("/bin/sh/app")])`)
		w.WhenISave("app")
		w.ThenItFailsWith(`layer "app": workdir("/bin/sh/app") failed`)
		w.ThenNothingIsSaved()
		w.ThenNoImageIsLeftBehind()
	})
}

// TestLayer_StepsApplyInOrder specifies that steps apply in order, and later steps see earlier changes.
func TestLayer_StepsApplyInOrder(t *testing.T) {
	t.Run("Files added later replace those added before", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="conf", include=["app.conf"])
			task(
			    name="generated",
			    base="busybox",
			    steps=[run(["sh", "-c", "mkdir /out && echo generated > /out/app.conf"])],
			    source="/out",
			    include=["app.conf"],
			)
			layer(name="app", base="busybox", steps=[add("conf", "/etc/app"), add("generated", "/etc/app")])
		`)
		w.GivenTheProjectHolds(files{"app.conf": "from the project\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/etc/app"), files{"/etc/app/app.conf": "generated\n"})
	})

	t.Run("env() reaches later steps and the image, build_env() only the layer's later steps", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(
			    name="base",
			    base="busybox",
			    steps=[
			        env(GREETING="hello"),
			        build_env(TOOL="cc"),
			        run(["sh", "-c", "echo $GREETING $TOOL > /during.txt"]),
			    ],
			)
			layer(
			    name="app",
			    base="base",
			    steps=[run(["sh", "-c", "echo $GREETING ${TOOL:-unset} > /inherited.txt"])],
			)
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHoldsAtLeast(files{
			"/during.txt":    "hello cc\n",
			"/inherited.txt": "hello unset\n",
		})
		w.ThenTheImageEnvironmentSets(environment{"GREETING": "hello"})
		w.ThenTheImageEnvironmentDoesNotSet("TOOL")
	})

	t.Run("workdir() makes its directory and is where later steps and the image run", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(
			    name="app",
			    base="busybox",
			    steps=[workdir("/srv/app"), run(["sh", "-c", "pwd > where.txt"]), workdir("/data")],
			)
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/srv"), files{"/srv/app/where.txt": "/srv/app\n"})
		w.ThenTheImageHasTheDirectory("/data")
		w.ThenTheImageHoldsNothing(under("/data"))
		w.ThenTheImageWorkingDirectoryIs("/data")
	})

	t.Run("A layer works in its base's working directory", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="base", base="busybox", steps=[workdir("/srv/app")])
			layer(name="app", base="base", steps=[run(["sh", "-c", "pwd > /where.txt"])])
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHoldsAtLeast(files{"/where.txt": "/srv/app\n"})
		w.ThenTheImageWorkingDirectoryIs("/srv/app")
	})

	t.Run("workdir() makes its directory for the image's user, who can write to it", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenARegistry()
		w.WhenIPush("busybox", "<registry>/nonroot:v1") // a pull can be sent as it is
		w.ThenItSucceeds()
		w.GivenTheRegistrysImageRunsAs("<registry>/nonroot:v1", "1000:1000")
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			pull(name="nonroot", image="<registry>/nonroot", tag="v1")
			layer(name="app", base="nonroot", steps=[workdir("/work/dir"), run(["sh", "-c", "echo %s > made.txt"])])
		`, testenv.NewNonce()))
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageOwnersAre(owners{
			"/work":              "1000:1000",
			"/work/dir":          "1000:1000",
			"/work/dir/made.txt": "1000:1000",
		})
	})
}

// TestLayer_FilesKeepModes specifies that files keep their modes as a layer adds them.
func TestLayer_FilesKeepModes(t *testing.T) {
	t.Run("A layer adds a task's output with the modes it was selected with", func(t *testing.T) {
		w := newLayerWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(
			    name="make",
			    base="busybox",
			    steps=[run(["sh", "-c", """
			        mkdir -p /out/shared
			        echo tool > /out/tool
			        echo map > /out/tool.map
			        echo data > /out/shared/data
			        chmod 4750 /out/tool
			        chmod 1777 /out/shared
			    """])],
			    source="/out",
			    include=["**"],
			    exclude=["*.map"],
			    mode_mask=0o7777,
			)
			task(
			    name="masked",
			    base="busybox",
			    steps=[run(["sh", "-c", "mkdir /out && echo lib > /out/lib.so && chmod 4775 /out/lib.so"])],
			    source="/out",
			    include=["*.so"],
			)
			layer(name="app", base="busybox", steps=[add("make", "/app"), add("masked", "/app/lib")])
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{
			"/app/tool":        "tool\n",
			"/app/shared/data": "data\n",
			"/app/lib/lib.so":  "lib\n",
		})
		w.ThenTheImageModesAre(modes{
			"/app/tool":        0o4750,
			"/app/shared":      0o1777,
			"/app/shared/data": 0o644,
			"/app/lib/lib.so":  0o755,
		})
	})
}

// TestLayer_CommandAndEntrypoint specifies that a layer's command and entrypoint
// work as a Dockerfile's CMD and ENTRYPOINT.
func TestLayer_CommandAndEntrypoint(t *testing.T) {
	for _, example := range []struct {
		name       string
		base       string
		arguments  string   // the layer's, after its name and base
		entrypoint []string // none if nil
		command    []string // none if nil
	}{
		{
			name:    "A layer keeps its base's command",
			base:    "busybox",
			command: []string{"sh"},
		},
		{
			name:    "A layer keeps its built base's command",
			base:    "deps",
			command: []string{"echo", "from deps"},
		},
		{
			name:      "A command replaces its base's",
			base:      "busybox",
			arguments: `command=["echo", "hi"]`,
			command:   []string{"echo", "hi"},
		},
		{
			name:       "An entrypoint drops its base's command",
			base:       "busybox",
			arguments:  `entrypoint=["java", "-jar", "app.jar"]`,
			entrypoint: []string{"java", "-jar", "app.jar"},
		},
		{
			name:       "A command follows the entrypoint as arguments",
			base:       "busybox",
			arguments:  `entrypoint=["echo"], command=["hello"]`,
			entrypoint: []string{"echo"},
			command:    []string{"hello"},
		},
		{
			name:       "A layer keeps its base's entrypoint",
			base:       "server",
			arguments:  `command=["--port", "9"]`,
			entrypoint: []string{"java", "-jar", "server.jar"},
			command:    []string{"--port", "9"},
		},
		{
			name:      "An empty entrypoint removes its base's",
			base:      "server",
			arguments: `entrypoint=[]`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newLayerWorld(t)
			w.GivenTheBuildFileDeclares(`
				layer(name="deps", base="busybox", command=["echo", "from deps"])
				layer(name="server", base="busybox", entrypoint=["java", "-jar", "server.jar"])
			`)
			app := fmt.Sprintf(`layer(name="app", base=%q, %s)`, example.base, example.arguments)
			w.GivenTheBuildFileDeclares(app)
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheImageEntrypointIs(example.entrypoint...)
			w.ThenTheImageCommandIs(example.command...)
		})
	}
}
