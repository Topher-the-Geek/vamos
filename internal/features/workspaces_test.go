package features

import (
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newWorkspaceWorld starts a scenario of the workspaces feature, from its background:
// a workspace, a directory with a WORKSPACE file, whose packages are directories with BUILD files.
func newWorkspaceWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenTheProjectHolds(files{"WORKSPACE": ""})
	return w
}

// TestWorkspaces_LabelsNameOtherPackagesTargets specifies that labels name the targets of other packages
// wherever a target reference is accepted, such as base, add() and dep(), and that a referenced glob keeps paths
// relative to its own BUILD directory.
func TestWorkspaces_LabelsNameOtherPackagesTargets(t *testing.T) {
	t.Run("A layer is built on, and adds, another package's targets", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		nonce := testenv.NewNonce()
		w.GivenThePackageDeclares("lib", testenv.PullBusybox)
		w.GivenThePackageDeclares("lib", `
			glob(name="code", include=["lib.sh"])
			task(
			    name="make",
			    base="busybox",
			    steps=[add("code", "/src"), run(["sh", "-c", "mkdir /out && cp /src/lib.sh /out/made.sh"])],
			    source="/out",
			    include=["made.sh"],
			)
		`)
		w.GivenThePackageDeclares("app", `
			glob(name="code", include=["app.sh"])
			layer(
			    name="app",
			    base=dep("//lib:busybox"),
			    steps=[add("code", "/app"), add("//lib:make", "/app"), add("//lib:code", "/srv/lib")],
			)
		`)
		w.GivenTheProjectHolds(files{"lib/lib.sh": "echo " + nonce, "app/app.sh": "echo app"})
		w.GivenIWorkIn("app")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("//lib:make"): ran})
		w.ThenTheImageHolds(under("/app"), files{"/app/app.sh": "echo app", "/app/made.sh": "echo " + nonce})
		w.ThenTheImageHolds(under("/srv/lib"), files{"/srv/lib/lib.sh": "echo " + nonce})
	})

	t.Run("From the root, every target is named by its label", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenThePackageDeclares("lib", testenv.PullBusybox)
		w.GivenThePackageDeclares("app", fmt.Sprintf(`
			layer(name="app", base="//lib:busybox", steps=[run(["echo", %q])])
		`, testenv.NewNonce()))
		w.GivenIWorkIn("app")
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.GivenIWorkIn(".")
		w.WhenIBuild("//app:app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("//app:app"): reused})
	})
}

// TestWorkspaces_OnlyNeededTargetsLoad specifies that vamos loads only the packages and targets
// that the requested targets need.
func TestWorkspaces_OnlyNeededTargetsLoad(t *testing.T) {
	t.Run("Targets and packages nothing needs are neither run nor checked", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenThePackageDeclares("lib", testenv.PullBusybox)
		w.GivenThePackageDeclares("lib", `
			glob(name="docs", include=["README"]) # missing, but nothing needs it
		`)
		w.GivenThePackageDeclares("app", `
			layer(name="app", base="//lib:busybox")
			task(name="test", base="app", steps=[run(["false"])])
			layer(name="later", base=lambda ctx: "//broken:base") # named once configured, which it never is
		`)
		w.GivenThePackageDeclares("broken", `this isn't Starlark`)
		w.GivenIWorkIn("app")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("test"): notBuilt})
	})
}

// TestWorkspaces_RootIsTheNearestWorkspaceFile specifies that vamos searches for the nearest WORKSPACE
// in -C's directory and its ancestors, and that without one, -C's directory is the workspace root.
func TestWorkspaces_RootIsTheNearestWorkspaceFile(t *testing.T) {
	for _, example := range []struct {
		name       string
		workspaces files  // the WORKSPACE files, none for none
		lib        string // the content of the lib package that the app's label reaches
	}{
		{
			name:       "A WORKSPACE above -C is the root",
			workspaces: files{"WORKSPACE": ""},
			lib:        "outer",
		},
		{
			name:       "A WORKSPACE in -C is the root, rather than one above",
			workspaces: files{"WORKSPACE": "", "app/WORKSPACE": ""},
			lib:        "inner",
		},
		{
			name: "Without a WORKSPACE, -C is the root",
			lib:  "inner",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newBuildingWorld(t)
			w.GivenTheProjectHolds(example.workspaces)
			lib := `glob(name="code", include=["lib.txt"])`
			w.GivenThePackageDeclares("lib", lib)
			w.GivenThePackageDeclares("app/lib", lib)
			w.GivenThePackageDeclares("app", testenv.PullBusybox)
			w.GivenThePackageDeclares("app", `
				layer(name="app", base="busybox", steps=[add("//lib:code", "/srv/lib")])
			`)
			w.GivenTheProjectHolds(files{"lib/lib.txt": "outer", "app/lib/lib.txt": "inner"})
			w.GivenIWorkIn("app")
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheImageHolds(under("/srv/lib"), files{"/srv/lib/lib.txt": example.lib})
		})
	}
}

// TestWorkspaces_BuildVmsIsABuildFile specifies that a package's file may be named BUILD.vms,
// where the directory also holds a build directory, and that a directory named BUILD or build is never taken for a
// BUILD file.
func TestWorkspaces_BuildVmsIsABuildFile(t *testing.T) {
	t.Run("Packages with BUILD.vms files are built by label and from -C", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenTheProjectHolds(files{
			"BUILD.vms":       `layer(name="app", base="//react:site")`,
			"react/BUILD.vms": `layer(name="site", base="scratch", command=["serve"])`,
			"react/BUILD/out": "a build directory, as where build is found as BUILD without case",
			"react/build/out": "a build directory",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageCommandIs("serve")
		w.GivenIWorkIn("react")
		w.WhenISave("site")
		w.ThenItSucceeds()
		w.ThenTheImageCommandIs("serve")
	})
}

// TestWorkspaces_PackagesMayNameEachOthersTargets specifies that packages may name each other's targets:
// a package's BUILD file runs when another first names one of its targets, while that other file is still running,
// and can then name the other file's targets declared above that point.
// What loading catches, a target declared below that point or a cycle,
// is in internal/cli's TestPackagesNamingEachOther.
func TestWorkspaces_PackagesMayNameEachOthersTargets(t *testing.T) {
	for _, example := range []struct {
		name     string
		packages map[string]string // BUILD files, by package
		statuses statuses          // of the layers built
	}{
		{
			name: "Two packages build on each other's layers",
			packages: map[string]string{
				".":     "layer(name=\"y\", base=\"scratch\")\nlayer(name=\"app\", base=\"//scala:x\")",
				"scala": `layer(name="x", base="//:y")`,
			},
			statuses: statuses{layer("y"): ranOrReused, layer("//scala:x"): ranOrReused, layer("app"): ranOrReused},
		},
		{
			name: "Three packages in a ring build on each other's layers",
			packages: map[string]string{
				".": "layer(name=\"a\", base=\"scratch\")\nlayer(name=\"app\", base=\"//b:b\")",
				"b": `layer(name="b", base="//c:c")`,
				"c": `layer(name="c", base="//:a")`,
			},
			statuses: statuses{
				layer("a"):     ranOrReused,
				layer("//c:c"): ranOrReused,
				layer("//b:b"): ranOrReused,
				layer("app"):   ranOrReused,
			},
		},
		{
			name: "A function of ctx names a target declared below, once every BUILD file has run",
			packages: map[string]string{
				".":     "layer(name=\"app\", base=\"//scala:x\")\nlayer(name=\"y\", base=\"scratch\")",
				"scala": `layer(name="x", base=lambda ctx: "//:y")`,
			},
			statuses: statuses{layer("y"): ranOrReused, layer("//scala:x"): ranOrReused, layer("app"): ranOrReused},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newWorkspaceWorld(t)
			for dir, build := range example.packages {
				w.GivenThePackageDeclares(dir, build)
			}
			w.WhenIBuild("app")
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(example.statuses)
		})
	}

	t.Run("Packages may add each other's files", func(t *testing.T) {
		w := newWorkspaceWorld(t)
		w.GivenThePackageDeclares(".", testenv.PullBusybox)
		w.GivenThePackageDeclares(".", `
			glob(name="src", include=["src.txt"])
			layer(name="app", base="busybox", steps=[add("//scala:t", "/t")])
		`)
		w.GivenThePackageDeclares("scala", `
			task(name="t", base="//:busybox", steps=[add("//:src", "/s")], source="/s", include=["."])
		`)
		w.GivenTheProjectHolds(files{"src.txt": "from the root"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/t"), files{"/t/src.txt": "from the root"})
	})
}
