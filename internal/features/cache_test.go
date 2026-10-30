package features

import (
	"fmt"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/opencontainers/go-digest"
)

// newCacheWorld starts a scenario of the cache feature, from its background.
// vamos caches target results and reuses them when their effective inputs are unchanged.
// Delivery commands use the same cache as build.
// Scenarios declare targets that print a value new to each, so that no result from another scenario is reused.
// Its project pulls busybox.
func newCacheWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// depsSizeAndApp declares the layer "app", on the layer "deps", adding the files of the task "size",
// which both add the project's input.txt. Deps sets variables, and both print nonce, deps also as an error,
// so that a scenario can tell whether they ran and see what they printed. Scenarios change it with
// GivenTheBuildFileChanges to show which of its inputs run which targets again.
func depsSizeAndApp(nonce string) string {
	return fmt.Sprintf(`
		glob(name="input", include=["input.txt"])
		layer(
		    name="deps",
		    base="busybox",
		    steps=[
		        add("input", "/deps"),
		        build_env(LANG="C", TZ="UTC"),
		        env(GREETING="hello", NAME="vamos"),
		        run(["sh", "-c", "echo deps %[1]s; echo deps warning %[1]s >&2; echo %[1]s > /deps/nonce.txt"]),
		    ],
		)
		task(
		    name="size",
		    base="busybox",
		    steps=[
		        add("input", "/work"),
		        run(["sh", "-c", "echo size %[1]s; wc -c < /work/input.txt > /work/size.txt; echo %[1]s >/work/n.txt"]),
		    ],
		    source="/work",
		    include=["size.txt", "n.txt"],
		)
		layer(name="app", base="deps", steps=[add("size", "/app")])
	`, nonce)
}

// TestCache_UnchangedInputsAreReused specifies that vamos reuses target results while their effective inputs are
// unchanged, and that the CLI reports reuse and replays a cached target's previous output.
func TestCache_UnchangedInputsAreReused(t *testing.T) {
	t.Run("A second save reuses every target, replays its output and saves the same image", func(t *testing.T) {
		w := newCacheWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(depsSizeAndApp(nonce))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): ran, task("size"): ran, layer("app"): ran})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): reused, layer("app"): reused})
		w.ThenTheOutputIncludes("deps " + nonce)
		w.ThenTheOutputIncludes("size " + nonce)
		w.ThenTheErrorOutputIncludes("deps warning " + nonce)
		w.ThenTheImageIsTheOneSavedFirst()
	})

	t.Run("A glob and a task that select no files are reused", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			glob(name="none", include=["*.go"], allow_no_files=True)
			task(
			    name="empty",
			    base="busybox",
			    steps=[run(["sh", "-c", "echo %[1]s; mkdir /out"])],
			    source="/out",
			    include=["*"],
			    allow_no_files=True,
			)
			layer(name="app", base="busybox", steps=[add("none", "/app"), add("empty", "/app"), env(NONCE="%[1]s")])
		`, testenv.NewNonce()))
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("empty"): ran, layer("app"): ran})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("empty"): reused, layer("app"): reused})
		w.ThenTheImageIsTheOneSavedFirst()
	})

	for _, example := range []struct {
		name     string
		old, new string // a change to depsSizeAndApp
	}{
		{
			name: "Build variables declared in another order",
			old:  `build_env(LANG="C", TZ="UTC")`,
			new:  `build_env(TZ="UTC", LANG="C")`,
		},
		{
			name: "Variables declared in another order",
			old:  `env(GREETING="hello", NAME="vamos")`,
			new:  `env(NAME="vamos", GREETING="hello")`,
		},
		{
			name: "A task's patterns reordered and repeated",
			old:  `include=["size.txt", "n.txt"]`,
			new:  `include=["./n.txt", "size.txt", "n.txt"]`,
		},
	} {
		t.Run(example.name+" are reused", func(t *testing.T) {
			w := newCacheWorld(t)
			w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
			w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.GivenTheBuildFileChanges(example.old, example.new)
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): reused})
			w.ThenTheImageIsTheOneSavedFirst()
		})
	}

	t.Run("Variables declared in another order make the same image when built again", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`env(GREETING="hello", NAME="vamos")`, `env(NAME="vamos", GREETING="hello")`)
		w.WhenIBuild("deps", alongWith("app"), rebuilding())
		w.ThenItSucceeds()
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageIsTheOneSavedFirst()
	})
}

// TestCache_SharedByBuildAndDelivery specifies that delivery commands use the same cache as build,
// which builds the targets it names and those they use.
func TestCache_SharedByBuildAndDelivery(t *testing.T) {
	t.Run("Build builds only the targets named and those they use, for later commands to reuse", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIBuild("size")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("size"): ran, layer("deps"): notBuilt, layer("app"): notBuilt})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): ran, task("size"): reused, layer("app"): ran})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): reused, layer("app"): reused})
	})

	t.Run("A target named twice is built once", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIBuild("app", alongWith("size", "app"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran, task("size"): ran})
	})
}

// TestCache_ChangedInputsRunAgain specifies that a target runs again when its inputs change:
// its base image, configured steps and arguments, and the contents, modes and links of the files it adds.
func TestCache_ChangedInputsRunAgain(t *testing.T) {
	for _, example := range []struct {
		name     string
		holds    files    // the project's files then
		modes    modes    // their modes then
		statuses statuses // of the layer deps, the task size, and the layer app, on deps and adding the files of size
	}{
		{
			name:     "An added file's content",
			holds:    files{"input.txt": "HELLO\n"},
			statuses: statuses{layer("deps"): ran, task("size"): ran, layer("app"): ran},
		},
		{
			name:     "An added file's mode",
			modes:    modes{"input.txt": 0o600},
			statuses: statuses{layer("deps"): ran, task("size"): ran, layer("app"): ran},
		},
	} {
		t.Run(example.name+" runs its target again", func(t *testing.T) {
			w := newCacheWorld(t)
			w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
			w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.GivenTheProjectHolds(example.holds)
			w.GivenTheProjectModes(example.modes)
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(example.statuses)
		})
	}

	for _, example := range []struct {
		name     string
		old, new string   // a change to depsSizeAndApp
		statuses statuses // of the layer deps, the task size, and the layer app, on deps and adding the files of size
	}{
		{
			name:     "A run step's command",
			old:      `"echo deps `,
			new:      `"echo changed deps `,
			statuses: statuses{layer("deps"): ran, task("size"): reused, layer("app"): reused},
		},
		{
			name:     "A step added",
			old:      `add("input", "/deps"),`,
			new:      `build_env(LANG="C"), add("input", "/deps"),`,
			statuses: statuses{layer("deps"): ran, task("size"): reused, layer("app"): reused},
		},
		{
			name:     "A variable's value",
			old:      `GREETING="hello"`,
			new:      `GREETING="hola"`,
			statuses: statuses{layer("deps"): ran, task("size"): reused, layer("app"): ran},
		},
		{
			name:     "Where a file is added",
			old:      `add("input", "/deps")`,
			new:      `add("input", "/deps/input")`,
			statuses: statuses{layer("deps"): ran, task("size"): reused, layer("app"): ran},
		},
		{
			name:     "Who owns an added file",
			old:      `add("input", "/deps")`,
			new:      `add("input", "/deps", owner="nobody")`,
			statuses: statuses{layer("deps"): ran, task("size"): reused, layer("app"): ran},
		},
		{
			name:     "A task's selection limit",
			old:      `source="/work",`,
			new:      `source="/work", max_file_count=9,`,
			statuses: statuses{layer("deps"): reused, task("size"): ran, layer("app"): reused},
		},
	} {
		t.Run(example.name+" runs its target again", func(t *testing.T) {
			w := newCacheWorld(t)
			w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
			w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.GivenTheBuildFileChanges(example.old, example.new)
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(example.statuses)
		})
	}

	t.Run("A file edited while the build reads it is cached as it was read", func(t *testing.T) {
		w := newCacheWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			glob(name="input", include=["input.txt"])
			layer(name="app", base="busybox", steps=[add("input", "/app"), run(["cat", "/app/input.txt"])])
		`)
		w.GivenTheProjectHolds(files{"input.txt": nonce + " before\n"})
		w.GivenTheFileChangesWhileTheBuildReadsIt("input.txt", nonce+" edited\n")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(nonce + " edited")
		w.ThenTheImageHolds(under("/app"), files{"/app/input.txt": nonce + " edited\n"})
		w.GivenNoFileChangesWhileTheBuildReadsIt()
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheImageHolds(under("/app"), files{"/app/input.txt": nonce + " before\n"})
	})

	t.Run("An added link's target runs its target again", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			glob(name="site", include=["site"])
			layer(name="app", base="busybox", steps=[add("site", "/srv"), run(["echo", %q])])
		`, testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"site/index.html": "<h1>hi</h1>", "site/about.html": "<h1>us</h1>"})
		w.GivenTheProjectLinks(links{"site/home.html": "index.html"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheProjectLinks(links{"site/home.html": "about.html"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheImageLinks(links{"/srv/site/home.html": "about.html"})
	})

	t.Run("A layer's base runs it again", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="tools", base="busybox", steps=[run(["echo", "tools", %q])])
			layer(name="app", base="busybox", steps=[run(["echo", %[1]q])])
		`, testenv.NewNonce()))
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`layer(name="app", base="busybox"`, `layer(name="app", base="tools"`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("tools"): ran, layer("app"): ran})
	})
}

// TestCache_NamesAndModificationTimesAreNotInputs specifies that target names and source modification times
// are not inputs: touching a file or renaming a target does not force it to run again.
func TestCache_NamesAndModificationTimesAreNotInputs(t *testing.T) {
	t.Run("A renamed target is reused", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`name="deps"`, `name="dependencies"`)
		w.GivenTheBuildFileChanges(`base="deps"`, `base="dependencies"`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("dependencies"): reused, task("size"): reused})
		w.ThenTheImageIsTheOneSavedFirst()
	})

	t.Run("A touched file is reused", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheFileIsTouched("input.txt")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): reused, layer("app"): reused})
		w.ThenTheImageIsTheOneSavedFirst()
	})
}

// TestCache_ConsumersRunAgainWhenWhatTheyConsumeChanges specifies that consumers run again
// when the result they consume changes, and only then.
func TestCache_ConsumersRunAgainWhenWhatTheyConsumeChanges(t *testing.T) {
	t.Run("A task that makes other files runs the layers adding them again", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheBuildFileChanges(`wc -c < /work/input.txt`, `wc -l < /work/input.txt`) // lines, not bytes
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): ran, layer("app"): ran})
		w.ThenTheImageFileDiffersFromTheOneSavedFirst("/app/size.txt")
	})

	t.Run("A task that runs again but makes the same files leaves the layers adding them reused", func(t *testing.T) {
		w := newCacheWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			glob(name="input", include=["input.txt"])
			task(
			    name="size",
			    base="busybox",
			    steps=[
			        add("input", "/work"),
			        run(["sh", "-c", "echo %[1]s; wc -c < /work/input.txt > /work/size.txt"]),
			    ],
			    source="/work",
			    include=["size.txt"],
			)
			layer(name="sized", base="busybox", steps=[add("size", "/app"), run(["echo", "%[1]s"])])
			layer(name="app", base="sized")
		`, nonce))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheProjectHolds(files{"input.txt": "HELLO\n"}) // as many bytes, so the same size
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("size"): ran, layer("sized"): reused})
		w.ThenTheImageIsTheOneSavedFirst()
	})
}

// TestCache_FailuresAreNotCached specifies that a failed target does not produce a successful reusable result,
// but dependencies that succeeded before its failure remain cached.
func TestCache_FailuresAreNotCached(t *testing.T) {
	t.Run("A failed target runs again, and the targets before it are reused", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="deps", base="busybox", steps=[run(["echo", %q])])
			layer(name="broken", base="deps", steps=[run(["false"])])
			layer(name="app", base="broken")
		`, testenv.NewNonce()))
		w.WhenISave("app")
		w.ThenItFailsWith(`layer "broken": run(["false"]) failed`)
		w.ThenTheTargetsWere(statuses{layer("deps"): ran, layer("broken"): ran})
		w.WhenISave("app")
		w.ThenItFailsWith(`layer "broken": run(["false"]) failed`)
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, layer("broken"): ran})
		w.ThenNothingIsSaved()
		w.ThenNoImageIsLeftBehind()
	})
}

// TestCache_RebuildRefreshesTheNamedTargets specifies that --rebuild runs the targets it names again,
// reuses their dependencies when possible, and replaces their cached results.
func TestCache_RebuildRefreshesTheNamedTargets(t *testing.T) {
	t.Run("A rebuilt target replaces its cached result, and the targets it uses are reused", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="base", base="busybox", steps=[run(["echo", %q])])
			layer(name="deps", base="base", steps=[run(["sh", "-c", "cat /proc/sys/kernel/random/uuid > /id"])])
			layer(name="app", base="deps")
		`, testenv.NewNonce()))
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.WhenIBuild("deps", rebuilding())
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("base"): reused, layer("deps"): rebuilt})
		w.ThenTheOutputDoesNotInclude("pulling again")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, layer("app"): ran})
		w.ThenTheImageFileDiffersFromTheOneSavedFirst("/id")
	})

	t.Run("Rebuilding every target makes the same image", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.WhenIBuild("deps", alongWith("size", "app"), rebuilding())
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): rebuilt, task("size"): rebuilt, layer("app"): rebuilt})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageIsTheOneSavedFirst()
	})

	t.Run("Rebuilding a pull resolves its tag again", func(t *testing.T) {
		w := newCacheWorld(t)
		w.WhenIBuild("busybox", rebuilding())
		w.ThenItSucceeds()
		w.ThenTheOutputIs("pull \"busybox\": pulling again\n")
	})
}

// TestCache_TimestampsComeFromBasesAndTasks specifies that the files a target adds, and the resulting image,
// use the latest timestamp from its base and any tasks whose files it adds.
func TestCache_TimestampsComeFromBasesAndTasks(t *testing.T) {
	t.Run("A layer's files and image take the time its pulled base was created", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("busybox")
		w.ThenItSucceeds()
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageWasCreatedWhenTheOneSavedFirstWas()
		w.ThenTheImageFilesWereModifiedWhenItWasCreated("/deps/nonce.txt", "/app/size.txt")
	})
}

// TestCache_AddedTasksTimestampIsAnInput specifies that an added task's timestamp is an input of the target
// that adds its files, even when their contents are unchanged.
func TestCache_AddedTasksTimestampIsAnInput(t *testing.T) {
	t.Run("A task rebuilt with a later timestamp runs the layers adding its files again, at that time",
		func(t *testing.T) {
			w := newCacheWorld(t)
			first := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
			later := first.Add(time.Hour)
			w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
			w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
			w.WhenISave("app", stampedAt(first.Format(time.RFC3339)))
			w.ThenItSucceeds()
			w.WhenIBuild("size", rebuilding(), stampedAt(later.Format(time.RFC3339)))
			w.ThenItSucceeds()
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): reused, layer("app"): ran})
			w.ThenTheImageFilesWereModifiedAt(later, "/app/size.txt")
			w.ThenTheImageWasCreatedAt(later)
		})
}

// TestCache_TimestampIsAMinimumForTargetsThatRun specifies that --timestamp sets a minimum time for newly executed
// targets, and is not a cache input, so a cached result keeps its previous timestamp.
func TestCache_TimestampIsAMinimumForTargetsThatRun(t *testing.T) {
	t.Run("Targets that run are stamped, and reused with that stamp whatever the next", func(t *testing.T) {
		w := newCacheWorld(t)
		stamp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("app", stampedAt(stamp.Format(time.RFC3339)))
		w.ThenItSucceeds()
		w.ThenTheImageFilesWereModifiedAt(stamp, "/deps/nonce.txt", "/app/size.txt")
		w.ThenTheImageWasCreatedAt(stamp)
		for _, next := range []time.Time{time.Unix(0, 0), stamp.Add(time.Hour)} {
			w.WhenISave("app", stampedAt(fmt.Sprint(next.Unix())))
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): reused, layer("app"): reused})
			w.ThenTheImageFilesWereModifiedAt(stamp, "/deps/nonce.txt", "/app/size.txt")
			w.ThenTheImageWasCreatedAt(stamp)
		}
	})

	t.Run("A timestamp before a target's base gives way to the base's", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenISave("busybox")
		w.ThenItSucceeds()
		w.WhenISave("app", stampedAt("0"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): ran})
		w.ThenTheImageWasCreatedWhenTheOneSavedFirstWas()
		w.ThenTheImageFilesWereModifiedWhenItWasCreated("/deps/nonce.txt", "/app/size.txt")
	})
}

// TestCache_ResultsThatCantBeUsedAreBuiltAgain specifies that a cached result that can't be used, since its image or
// files were removed, or are corrupt, is reported and built again, rather than failing the build; files of the project
// that a glob captured, which only the client can capture again, fail the build, saying to run it again.
// Scenarios damage the build cache, so each has stores of its own.
func TestCache_ResultsThatCantBeUsedAreBuiltAgain(t *testing.T) {
	t.Run("A task whose cached files are corrupt is built again, and what needed them tried again", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenStoresOfTheScenariosOwn()
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(depsSizeAndApp(nonce))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.GivenTheCachedBlobOfIsCorrupt(nonce + "\n") // the task's n.txt, which app adds
		w.WhenIBuild("app", rebuilding())
		w.ThenItSucceeds()
		w.ThenTheTargetWasShownAs(task("size"), "cached", "building again, since its cached files are corrupt: ")
		w.ThenTheTargetWasShownAs(layer("app"), "rebuilding", "rebuilding") // tried again, once the task's files are
		w.WhenIBuild("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("deps"): reused, task("size"): reused, layer("app"): reused})
		w.WhenIExport("size") // from the files made again, which a corrupt blob would fail
		w.ThenItSucceeds()
		w.ThenTheExportHolds(files{"size.txt": "6\n", "n.txt": nonce + "\n"})
	})

	t.Run("A task whose cached files are a malformed tree is built again, saying why", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenStoresOfTheScenariosOwn()
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIBuild("size")
		w.ThenItSucceeds()
		w.GivenEveryCachedTasksFilesAreAMalformedTree()
		w.WhenIBuild("size")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`task "size": building, since the cached result can't be used: ` +
			"its files can't be used from the cache: ")
		w.ThenTheOutputIncludes(`is corrupt: it isn't a valid tree: "../x"`)
	})

	t.Run("A project file whose captured copy is corrupt fails, saying to run the build again, which repairs it",
		func(t *testing.T) {
			w := newCacheWorld(t)
			w.GivenStoresOfTheScenariosOwn()
			nonce := testenv.NewNonce()
			input := "hello " + nonce + "\n"
			w.GivenTheBuildFileDeclares(depsSizeAndApp(nonce))
			w.GivenTheProjectHolds(files{"input.txt": input})
			w.WhenIBuild("deps")
			w.ThenItSucceeds()
			w.GivenTheCachedBlobOfIsCorrupt(input)
			w.WhenIBuild("deps", rebuilding())
			w.ThenItFailsWith(`layer "deps": add("input", "/deps") failed: copying sources from the build cache: `+
				"input.txt: blob "+digest.FromString(input).String()+" is corrupt", "captured and submitted again")
			w.WhenIBuild("deps", rebuilding())
			w.ThenItSucceeds()
			w.WhenISave("deps")
			w.ThenItSucceeds()
			w.ThenTheImageHolds(under("/deps"), files{"/deps/input.txt": input, "/deps/nonce.txt": nonce + "\n"})
		})

	t.Run("A layer whose cached log is corrupt is built again, saying why", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenStoresOfTheScenariosOwn()
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIBuild("deps")
		w.ThenItSucceeds()
		w.GivenEveryCachedLogIsCorrupt()
		w.WhenIBuild("deps")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "deps": building, since the cached result can't be used: ` +
			"its output can't be used from the cache: ")
		w.ThenTheOutputIncludes("is corrupt")
	})

	t.Run("A layer whose cached log is malformed is built again, saying why", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenStoresOfTheScenariosOwn()
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIBuild("deps")
		w.ThenItSucceeds()
		w.GivenEveryCachedLogIsMalformed()
		w.WhenIBuild("deps")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "deps": building, since the cached result can't be used: ` +
			"its output can't be used from the cache: ")
		w.ThenTheOutputIncludes("it isn't a whole log")
	})

	t.Run("A layer whose cached image was removed is built again, saying why", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenStoresOfTheScenariosOwn()
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIBuild("busybox") // pulled first, so that the next command's images are the layers' alone
		w.ThenItSucceeds()
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheImagesTheLastCommandCachedAreRemoved()
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "deps": building, since the cached result can't be used: its image `)
		w.ThenTheOutputIncludes("is no longer in the container store")
		w.ThenTheTargetsWere(statuses{task("size"): reused})
	})

	t.Run("Targets whose cached files were removed are built again, saying why", func(t *testing.T) {
		w := newCacheWorld(t)
		w.GivenStoresOfTheScenariosOwn()
		w.GivenTheBuildFileDeclares(depsSizeAndApp(testenv.NewNonce()))
		w.GivenTheProjectHolds(files{"input.txt": "hello\n"})
		w.WhenIBuild("busybox") // pulled first, so that the next command's blobs are the targets' alone
		w.ThenItSucceeds()
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.GivenTheBlobsTheLastCommandCachedAreRemoved()
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes(`layer "deps": building, since the cached result can't be used: ` +
			"its output can't be used from the cache: blob sha256:")
		w.ThenTheOutputIncludes(`task "size": building, since the cached result can't be used: ` +
			"its files can't be used from the cache: tree sha256:")
	})
}
