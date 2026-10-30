package features

import (
	"cmp"
	"fmt"
	"runtime"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/containerd/platforms"
)

// newPlatformWorld starts a scenario of the platforms feature, from its background: a project that pulls busybox.
// Scenarios build for this machine's platform and another, but run commands only natively,
// so that they need no emulator.
func newPlatformWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newPlatformMistakeWorld starts a scenario of the platforms feature whose BUILD file gives a platform wrongly,
// which loading catches before anything runs, so that it needs no buildah.
// Its project pulls busybox.
func newPlatformMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// machine returns what `uname -m` prints on this machine, or skips the scenario on a machine the scenarios don't know.
func machine(t *testing.T) string {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	t.Skipf("no machine name for %s", runtime.GOARCH)
	return ""
}

// platformInTheEnvironment declares the layer "app", which sets its image's variables to what ctx reads of the
// platforms: PLATFORM, OS, ARCH and VARIANT, or "none", of the target's, and EXEC, the exec platform.
const platformInTheEnvironment = `
	layer(
	    name="app",
	    base="busybox",
	    steps=lambda ctx: [env(
	        PLATFORM=ctx.platform,
	        OS=ctx.os,
	        ARCH=ctx.arch,
	        VARIANT=ctx.variant or "none",
	        EXEC=ctx.exec_platform,
	    )],
	)
`

// TestPlatforms_TargetsTakeTheRequestedPlatform specifies that targets inherit the requested platform through their
// dependencies, and that the command defaults to the host's platform unless --platform is given.
func TestPlatforms_TargetsTakeTheRequestedPlatform(t *testing.T) {
	for _, example := range []struct {
		name     string
		options  []option
		platform string
	}{
		{
			name:     "Without --platform, a layer is for this machine's platform",
			platform: thisPlatform(),
		},
		{
			name:     "With --platform, a layer is for that platform",
			options:  []option{forPlatform(anotherPlatform())},
			platform: anotherPlatform(),
		},
		{
			name:     "With --platform, a layer is for that platform's variant",
			options:  []option{forPlatform("linux/arm/v7")},
			platform: "linux/arm/v7",
		},
		{
			name:     "With --platform, a layer is for that platform's variant, rather than the usual one",
			options:  []option{forPlatform("linux/arm/v6")},
			platform: "linux/arm/v6",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPlatformWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", command=["true"])`)
			w.WhenISave("app", example.options...)
			w.ThenItSucceeds()
			w.ThenTheImageIsFor(example.platform)
		})
	}
}

// TestPlatforms_FunctionsOfCtxReadThePlatform specifies that functions of ctx read the target's platform, os, arch
// and variant, and the platform executing build steps natively.
func TestPlatforms_FunctionsOfCtxReadThePlatform(t *testing.T) {
	t.Run("By default, a layer reads this machine's platform, as the target's and the exec platform",
		func(t *testing.T) {
			w := newPlatformWorld(t)
			w.GivenTheBuildFileDeclares(platformInTheEnvironment)
			w.WhenISave("app")
			w.ThenItSucceeds()
			p := platforms.MustParse(thisPlatform())
			w.ThenTheImageEnvironmentSets(environment{
				"PLATFORM": thisPlatform(),
				"OS":       p.OS,
				"ARCH":     p.Architecture,
				"VARIANT":  cmp.Or(p.Variant, "none"),
				"EXEC":     thisPlatform(),
			})
		})

	t.Run("With --platform, a layer reads that platform, and this machine's as the exec platform", func(t *testing.T) {
		w := newPlatformWorld(t)
		w.GivenTheBuildFileDeclares(platformInTheEnvironment)
		w.WhenISave("app", forPlatform("linux/arm/v7"))
		w.ThenItSucceeds()
		w.ThenTheImageEnvironmentSets(environment{
			"PLATFORM": "linux/arm/v7",
			"OS":       "linux",
			"ARCH":     "arm",
			"VARIANT":  "v7",
			"EXEC":     thisPlatform(),
		})
	})
}

// TestPlatforms_ExecPlatformRunsNativeTools specifies that EXEC_PLATFORM asks dep() for the executor's platform,
// rather than the target's, so a task runs native tools while producing files for the requested platform, and that
// such a task shares one result across platforms when its configured work is identical, but not when it reads the
// target platform.
func TestPlatforms_ExecPlatformRunsNativeTools(t *testing.T) {
	t.Run("Tasks on the exec platform run natively for either platform, and share their work if they can",
		func(t *testing.T) {
			w := newPlatformWorld(t)
			uname := machine(t)
			nonce := testenv.NewNonce()
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				task(
				    name="cross",
				    base=dep("busybox", platform=EXEC_PLATFORM),
				    steps=lambda ctx: [
				        run(["sh", "-c", "mkdir /out && echo %[1]s $(uname -m) %%s > /out/cross.txt" %% ctx.arch]),
				    ],
				    source="/out",
				    include=["cross.txt"],
				)
				task(
				    name="shared",
				    base=lambda ctx: dep("busybox", platform=ctx.exec_platform),
				    steps=[run(["sh", "-c", "mkdir /out && echo %[1]s $(uname -m) > /out/shared.txt"])],
				    source="/out",
				    include=["shared.txt"],
				)
				layer(name="files", base="scratch", steps=[add("cross", "/"), add("shared", "/")])
			`, nonce))
			for _, step := range []struct {
				platform string
				statuses statuses
			}{
				{
					platform: thisPlatform(),
					statuses: statuses{task("cross"): ran, task("shared"): ran, layer("files"): ran},
				},
				{
					platform: anotherPlatform(),
					// cross reads the target's platform, so it runs again; the first build's result of shared serves.
					statuses: statuses{task("cross"): ran, task("shared"): reused, layer("files"): ran},
				},
			} {
				w.WhenISave("files", forPlatform(step.platform))
				w.ThenItSucceeds()
				w.ThenTheTargetsWere(step.statuses)
				w.ThenTheImageIsFor(step.platform)
				w.ThenTheImageHolds(everywhere, files{
					"/cross.txt":  nonce + " " + uname + " " + platforms.MustParse(step.platform).Architecture + "\n",
					"/shared.txt": nonce + " " + uname + "\n",
				})
			}
		})
}

// TestPlatforms_SavingAnIndexSavesThisMachinesImage specifies that saving an index builds and saves only the image
// for the host's platform.
func TestPlatforms_SavingAnIndexSavesThisMachinesImage(t *testing.T) {
	for _, example := range []struct {
		name      string
		platforms []string
	}{
		{
			name:      "An index saves this machine's image when it is first in the list",
			platforms: []string{thisPlatform(), anotherPlatform()},
		},
		{
			name:      "An index saves this machine's image when it is second in the list",
			platforms: []string{anotherPlatform(), thisPlatform()},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPlatformWorld(t)
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				layer(name="app", base="scratch", command=[%q])
				index(name="all", layer="app", platforms=[%q, %q])
			`, testenv.NewNonce(), example.platforms[0], example.platforms[1]))
			w.WhenISave("all")
			w.ThenItSucceeds()
			w.ThenTheImageIsForThisMachine()
			w.ThenTheTargetsWere(statuses{layer("app"): ranOrReused}) // this machine's, which names no platform
			w.ThenTheTargetWasBuiltInOneConfiguration(layer("app"))
		})
	}

	t.Run("Saving an index configures only this machine's target, so another's mistake fails only a build of all",
		func(t *testing.T) {
			w := newPlatformWorld(t)
			nonce := testenv.NewNonce()
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				layer(name="app", base="scratch", command=["app", %q])
				layer(name="broken", base=lambda ctx: fail("configured broken for " + ctx.platform))
				index(name="all", layer={%q: "broken", %q: "app"})
			`, nonce, anotherPlatform(), thisPlatform()))
			w.WhenISave("all")
			w.ThenItSucceeds()
			w.ThenTheImageCommandIs("app", nonce)
			w.ThenTheTargetsWere(statuses{layer("broken"): notBuilt})
			w.ThenTheOutputDoesNotInclude("broken")
			w.WhenIBuild("all")
			w.ThenItFailsWith("configured broken for " + anotherPlatform())
		})
}

// TestPlatforms_APlatformIsOsArchAndVariant specifies that a platform is os/arch or os/arch/variant,
// such as linux/arm64 or linux/arm/v7, or EXEC_PLATFORM for the executor's.
func TestPlatforms_APlatformIsOsArchAndVariant(t *testing.T) {
	for _, example := range []struct {
		name     string
		platform string
		error    string // after dep()'s position
	}{
		{
			name:     "A platform that isn't os/arch fails",
			platform: `"x86"`,
			error:    `dep: platform "x86": want os/arch or os/arch/variant, such as "linux/arm64"`,
		},
		{
			name:     "A platform with an empty part fails",
			platform: `"linux//amd64"`,
			error:    `dep: platform "linux//amd64": want os/arch or os/arch/variant, such as "linux/arm64"`,
		},
		{
			name:     "A platform with a part after its variant fails",
			platform: `"linux/arm/v7/x"`,
			error:    `dep: platform "linux/arm/v7/x": want os/arch or os/arch/variant, such as "linux/arm64"`,
		},
		{
			name:     "A platform that isn't a string fails",
			platform: `64`,
			error:    `dep: platform is int, want a platform, such as "linux/arm64", or EXEC_PLATFORM`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPlatformMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", base=dep("busybox", platform=` + example.platform + `))`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:27: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestPlatforms_GlobsAreSharedAcrossPlatforms specifies that globs are shared across platforms and cannot read
// ctx.platform, ctx.os, ctx.arch, or ctx.variant.
func TestPlatforms_GlobsAreSharedAcrossPlatforms(t *testing.T) {
	for _, field := range []string{"platform", "os", "arch", "variant"} {
		t.Run("A glob that reads ctx."+field+" fails", func(t *testing.T) {
			w := newPlatformMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`
				glob(name="code", include=lambda ctx: [str(ctx.` + field + `)])
				layer(name="app", base="busybox", steps=[add("code", "/")])
			`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:47: ctx has no " + field + " in a glob, " +
				"whose files are the same for every platform (configuring //:code)")
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestPlatforms_VariantsReachTheImage specifies that a platform's variant, as in linux/arm/v7, reaches functions of ctx
// and the image built for it.
func TestPlatforms_VariantsReachTheImage(t *testing.T) {
	t.Run("A layer saved for a variant is for it, and its functions read it", func(t *testing.T) {
		w := newPlatformWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="scratch", command=lambda ctx: ["app", %q, ctx.variant])
		`, nonce))
		w.WhenISave("app", forPlatform("linux/arm/v7"))
		w.ThenItSucceeds()
		w.ThenTheImageIsFor("linux/arm/v7")
		w.ThenTheImageCommandIs("app", nonce, "v7")
	})
}
