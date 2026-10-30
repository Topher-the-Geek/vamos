package features

import (
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newGlobWorld starts a scenario of the glob feature, from its background.
// A glob selects files relative to the directory containing its BUILD file, as input for add().
// Scenarios see what a glob selects in the image of a layer that adds it.
// Its project pulls busybox.
func newGlobWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newGlobMistakeWorld starts a scenario of the glob feature whose glob fails as it is read, before any target runs,
// so that it needs no buildah and leaves no image behind.
// Its project pulls busybox.
func newGlobMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// TestGlob_PathsStayRelativeToTheBuildDirectory specifies that a glob selects files relative to the directory
// containing its BUILD file, and that their paths stay relative to it, even when a pattern has a leading directory.
func TestGlob_PathsStayRelativeToTheBuildDirectory(t *testing.T) {
	t.Run("A directory selects its contents recursively, beneath its own name", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="site", include=["site"])
			layer(name="app", base="busybox", steps=[add("site", "/srv")])
		`)
		w.GivenTheProjectHolds(files{
			"site/index.html": "<h1>hi</h1>",
			"site/css/a.css":  "h1 {}",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/srv"), files{
			"/srv/site/index.html": "<h1>hi</h1>",
			"/srv/site/css/a.css":  "h1 {}",
		})
	})

	t.Run("A file keeps its path however many patterns and globs select it", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["main.sh", "lib/util.sh", "lib"])
			glob(name="docs", include=["./docs/README.md", "main.sh"])
			layer(name="app", base="busybox", steps=[add("code", "/app"), add("docs", "/app")])
		`)
		w.GivenTheProjectHolds(files{
			"main.sh":        "echo main",
			"lib/util.sh":    "echo util",
			"lib/more.sh":    "echo more",
			"docs/README.md": "# app",
			"docs/other.md":  "not included",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{
			"/app/main.sh":        "echo main",
			"/app/lib/util.sh":    "echo util",
			"/app/lib/more.sh":    "echo more",
			"/app/docs/README.md": "# app",
		})
	})

	t.Run("A dot selects the whole directory", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="all", include=["."], exclude=["BUILD"])
			layer(name="app", base="busybox", steps=[add("all", "/app")])
		`)
		w.GivenTheProjectHolds(files{
			"main.sh":     "echo main",
			"lib/util.sh": "echo util",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{
			"/app/main.sh":     "echo main",
			"/app/lib/util.sh": "echo util",
		})
	})
}

// TestGlob_PatternsSelectFiles specifies that a glob's include and exclude patterns choose its files:
// * matches within one path component, ** matches zero or more directories,
// and an exclusion matching a directory omits all of it.
func TestGlob_PatternsSelectFiles(t *testing.T) {
	t.Run("Patterns select files at any depth, less those excluded", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["src/**/*.ts", "*.json"], exclude=["**/*.test.ts"])
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.GivenTheProjectHolds(files{
			"package.json":     "{}",
			"README.md":        "# app",
			"src/main.ts":      "main",
			"src/main.test.ts": "test",
			"src/lib/util.ts":  "util",
			"src/lib/util.js":  "compiled",
			"lib/other.ts":     "other",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{
			"/app/package.json":    "{}",
			"/app/src/main.ts":     "main",
			"/app/src/lib/util.ts": "util",
		})
	})

	t.Run("A star matches names that begin with a dot", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="config", include=["*rc"])
			layer(name="app", base="busybox", steps=[add("config", "/app")])
		`)
		w.GivenTheProjectHolds(files{
			".npmrc":    "registry",
			"bashrc":    "alias",
			"README.md": "# app",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{
			"/app/.npmrc": "registry",
			"/app/bashrc": "alias",
		})
	})

	t.Run("An excluded directory is left out with everything in it", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["."], exclude=["BUILD", "**/node_modules"])
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.GivenTheProjectHolds(files{
			"main.js":                      "main",
			"node_modules/dep/index.js":    "dep",
			"lib/node_modules/dep/dep.js":  "nested dep",
			"lib/util.js":                  "util",
			"lib/node_modules_notes/a.txt": "kept",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{
			"/app/main.js":                      "main",
			"/app/lib/util.js":                  "util",
			"/app/lib/node_modules_notes/a.txt": "kept",
		})
	})

	for _, example := range []struct {
		name    string
		include string
		error   string // after the glob's position
	}{
		{
			name:    "A ** within a name fails, saying to make it a whole part of the path",
			include: "src/**.ts",
			error:   `include "src/**.ts": "**" must be a whole part of the path, as in "src/**/*.ts"`,
		},
		{
			name:    "A pattern that doesn't parse fails",
			include: "src/[a",
			error:   `include "src/[a": syntax error in pattern`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newGlobMistakeWorld(t)
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`glob(name="code", include=[%q])`, example.include))
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app")])`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:5: glob: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestGlob_ModeMaskRetainsPermissionBits specifies that a glob's mode_mask masks its files' and directories'
// existing permission bits, rather than assigning a fixed mode, and retains those of 0o755 by default.
func TestGlob_ModeMaskRetainsPermissionBits(t *testing.T) {
	for _, example := range []struct {
		name     string
		modeMask string // the glob's mode_mask argument, if any
		want     modes
	}{
		{
			name: "The default mask retains the permission bits of 0o755",
			want: modes{
				"/app/src/main.sh":     0o755,
				"/app/src/main.ts":     0o644,
				"/app/src/lib":         0o755,
				"/app/src/lib/util.ts": 0o640,
			},
		},
		{
			name:     "A mask of 0o777 keeps every permission bit",
			modeMask: ", mode_mask=0o777",
			want: modes{
				"/app/src/main.sh":     0o777,
				"/app/src/main.ts":     0o666,
				"/app/src/lib":         0o777,
				"/app/src/lib/util.ts": 0o640,
			},
		},
		{
			name:     "A mask of 0o700 keeps only the owner's",
			modeMask: ", mode_mask=0o700",
			want: modes{
				"/app/src/main.sh":     0o700,
				"/app/src/main.ts":     0o600,
				"/app/src/lib":         0o700,
				"/app/src/lib/util.ts": 0o600,
			},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newGlobWorld(t)
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`glob(name="code", include=["src"]%s)`, example.modeMask))
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app")])`)
			w.GivenTheProjectHolds(files{
				"src/main.sh":     "echo main",
				"src/main.ts":     "main",
				"src/lib/util.ts": "util",
			})
			w.GivenTheProjectModes(modes{
				"src/main.sh":     0o777,
				"src/main.ts":     0o666,
				"src/lib":         0o777,
				"src/lib/util.ts": 0o640,
			})
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheImageModesAre(example.want)
		})
	}

	t.Run("The directories that lead to a pattern's matches are not selected, nor their modes", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["src/lib/*.ts"], mode_mask=0o777)
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.GivenTheProjectHolds(files{"src/lib/util.ts": "util"})
		w.GivenTheProjectModes(modes{"src": 0o700, "src/lib": 0o700})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{"/app/src/lib/util.ts": "util"})
		w.ThenTheImageModesAre(modes{"/app/src": 0o755, "/app/src/lib": 0o755})
	})
}

// TestGlob_LinksAreCopiedAsLinks specifies that links matched by wildcards or found within an included directory
// are copied as links and never followed.
func TestGlob_LinksAreCopiedAsLinks(t *testing.T) {
	t.Run("Links within an included directory are copied as links, wherever they point", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="site", include=["site"])
			layer(name="app", base="busybox", steps=[add("site", "/srv")])
		`)
		w.GivenTheProjectHolds(files{
			"site/index.html":  "<h1>hi</h1>",
			"shared/style.css": "h1 {}",
		})
		w.GivenTheProjectLinks(links{
			"site/home.html": "index.html",
			"site/secret":    "/etc/hostname",
			"site/lib":       "../shared",
		})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImagePathsAre(under("/srv"),
			"/srv/site", "/srv/site/index.html", "/srv/site/home.html", "/srv/site/secret", "/srv/site/lib")
		w.ThenTheImageHoldsAtLeast(files{"/srv/site/index.html": "<h1>hi</h1>"})
		w.ThenTheImageLinks(links{
			"/srv/site/home.html": "index.html",
			"/srv/site/secret":    "/etc/hostname",
			"/srv/site/lib":       "../shared",
		})
	})

	t.Run("Links matched by a wildcard are copied as links", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="docs", include=["*.md"])
			layer(name="app", base="busybox", steps=[add("docs", "/app")])
		`)
		w.GivenTheProjectHolds(files{"README.md": "# app"})
		w.GivenTheProjectLinks(links{"INDEX.md": "README.md"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImagePathsAre(under("/app"), "/app/README.md", "/app/INDEX.md")
		w.ThenTheImageHoldsAtLeast(files{"/app/README.md": "# app"})
		w.ThenTheImageLinks(links{"/app/INDEX.md": "README.md"})
	})
}

// TestGlob_IncludesMustNotBeLinks specifies that in each include, every path component before the first wildcard
// must exist and must not be a symbolic link, even when allow_no_files = True.
// The links point within the project, so that following one would select a file rather than fail.
func TestGlob_IncludesMustNotBeLinks(t *testing.T) {
	for _, example := range []struct {
		name    string
		include string
		links   links
		error   string // after the glob's position
	}{
		{
			name:    "An include that is a link fails",
			include: "lib",
			links:   links{"lib": "shared"},
			error:   `include "lib" is a symbolic link; include what it links to instead`,
		},
		{
			name:    "An include whose parent is a link fails",
			include: "external/secret",
			links:   links{"external": "shared"},
			error:   `include "external/secret" has symbolic link parent "/project/external"`,
		},
		{
			name:    "An include whose grandparent is a link fails",
			include: "linked/shared/secret",
			links:   links{"linked": "."},
			error:   `include "linked/shared/secret" has symbolic link parent "/project/linked"`,
		},
		{
			name:    "A link before the first wildcard fails",
			include: "external/*",
			links:   links{"external": "shared"},
			error:   `include "external/*": "external" is a symbolic link; include what it links to instead`,
		},
	} {
		for _, allow := range []bool{false, true} {
			name, options := example.name, ""
			if allow {
				name, options = name+", even allowing no files", ", allow_no_files=True"
			}
			build := fmt.Sprintf(`glob(name="code", include=[%q]%s)`, example.include, options)
			t.Run(name, func(t *testing.T) {
				w := newGlobMistakeWorld(t)
				w.GivenTheBuildFileDeclares(build)
				w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app")])`)
				w.GivenTheProjectHolds(files{"shared/secret": "private"})
				w.GivenTheProjectLinks(example.links)
				w.WhenISave("app")
				w.ThenItFailsWith("/project/BUILD:2:5: glob: " + example.error)
				w.ThenNoTargetRan()
				w.ThenNothingIsSaved()
			})
		}
	}

	t.Run("A missing file fails", func(t *testing.T) {
		w := newGlobMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["app.sh"])
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD:2:5: glob: include "app.sh" not found in /project`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	t.Run("A missing include fails, even allowing no files", func(t *testing.T) {
		w := newGlobMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["src/*.go"], allow_no_files=True)
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD:2:5: glob: include "src/*.go": "src" not found in /project`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestGlob_SelectionIsBounded specifies that a glob must select files unless allow_no_files = True,
// and no more files or bytes than its limits.
func TestGlob_SelectionIsBounded(t *testing.T) {
	t.Run("Allowing no files, a glob may select none", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["*.go"], allow_no_files=True)
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHoldsNothing(under("/app"))
	})

	for _, example := range []struct {
		name  string
		glob  string
		error string // after the glob's position
	}{
		{
			name:  "A glob that selects no files fails",
			glob:  `glob(name="code", include=["*.go"])`,
			error: `no files in /project match include "*.go"; pass allow_no_files=True`,
		},
		{
			name:  "A glob that selects a file larger than max_file_size fails",
			glob:  `glob(name="code", include=["*.sh"], max_file_size=7)`,
			error: `/project/lib.sh is 8 bytes, more than max_file_size=7`,
		},
		{
			name:  "A glob that selects more files than max_file_count fails",
			glob:  `glob(name="code", include=["*.sh"], max_file_count=1)`,
			error: `more than max_file_count=1 files in /project match`,
		},
		{
			name:  "A glob that selects more bytes than max_total_size fails",
			glob:  `glob(name="code", include=["*.sh"], max_total_size=10)`,
			error: "the files in /project that match total more than max_total_size=10 bytes",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newGlobMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.glob)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app")])`)
			w.GivenTheProjectHolds(files{"app.sh": "echo hi", "lib.sh": "echo lib"})
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:5: glob: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestGlob_ReadBeforeExecution specifies that vamos captures reachable globs once before execution,
// and that globs that no requested target uses are not read.
func TestGlob_ReadBeforeExecution(t *testing.T) {
	t.Run("A glob's mistake is reported before any target runs", func(t *testing.T) {
		w := newGlobMistakeWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="first", base="busybox", steps=[run(["echo", %q])])
			glob(name="code", include=["*.go"])
			layer(name="app", base="first", steps=[add("code", "/app")])
		`, testenv.NewNonce()))
		w.WhenIBuild("app")
		w.ThenItFailsWith(`/project/BUILD:3:5: glob: no files in /project match include "*.go"`)
		w.ThenTheTargetsWere(statuses{layer("first"): notBuilt})
	})

	t.Run("A glob's mistake leaves no image behind", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="first", base="busybox", steps=[run(["echo", %q])])
			glob(name="code", include=["*.go"])
			layer(name="app", base="first", steps=[add("code", "/app")])
		`, testenv.NewNonce()))
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD:3:5: glob: no files in /project match include "*.go"`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
		w.ThenNoImageIsLeftBehind()
	})

	t.Run("Every target that adds a glob gets its files as they were first read", func(t *testing.T) {
		w := newGlobWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(`
			glob(name="input", include=["input.txt"])
			layer(name="first", base="busybox", steps=[add("input", "/first")])
			layer(name="app", base="first", steps=[add("input", "/app")])
		`)
		w.GivenTheProjectHolds(files{"input.txt": nonce + " as read"})
		w.GivenTheFileChangesOnceTheBuildHasReadIt("input.txt", nonce+" edited")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/first"), files{"/first/input.txt": nonce + " as read"})
		w.ThenTheImageHolds(under("/app"), files{"/app/input.txt": nonce + " as read"})
	})

	t.Run("Only the mistake of a glob that a requested target uses is reported", func(t *testing.T) {
		w := newGlobMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="docs", include=["README"])
			glob(name="code", include=["app.sh"])
			layer(name="docs-layer", base="busybox", steps=[add("docs", "/docs")])
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.WhenISave("app")
		w.ThenItFailsWith(`/project/BUILD:3:5: glob: include "app.sh" not found in /project`)
		w.ThenTheCommandDoesNotSay("README")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	t.Run("A glob that no requested target uses is not read", func(t *testing.T) {
		w := newGlobWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="unused", include=["missing/*.go"])
			glob(name="code", include=["app.sh"])
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/app"), files{"/app/app.sh": "echo hi"})
	})
}

// TestGlob_NameAndANonemptyIncludeAreRequired specifies that a glob's name and a nonempty include list are required.
func TestGlob_NameAndANonemptyIncludeAreRequired(t *testing.T) {
	for _, example := range []struct {
		name  string
		glob  string
		error string // after the glob's position
	}{
		{
			name:  "A glob without include fails",
			glob:  `glob(name="code")`,
			error: "missing argument for include",
		},
		{
			name:  "A glob with an empty include fails",
			glob:  `glob(name="code", include=[])`,
			error: "include must not be empty",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newGlobMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.glob)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app")])`)
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:5: glob: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestGlob_PatternsStayWithinTheRoot specifies that patterns cannot be empty or resolve outside the selection root,
// the BUILD file's directory for a glob, after normalizing . and .. components: use . to select the whole directory.
func TestGlob_PatternsStayWithinTheRoot(t *testing.T) {
	for _, example := range []struct {
		name  string
		glob  string
		error string // after the glob's position
	}{
		{
			name:  "An empty include fails, saying to use . for the whole directory",
			glob:  `glob(name="code", include=["app.sh", ""])`,
			error: `include[1] is empty; use "." for the whole directory`,
		},
		{
			name:  "An empty exclude fails",
			glob:  `glob(name="code", include=["."], exclude=[""])`,
			error: "exclude[0] is empty",
		},
		{
			name:  "An absolute include fails",
			glob:  `glob(name="code", include=["/etc/passwd"])`,
			error: `include "/etc/passwd" must be a path relative to /project`,
		},
		{
			name:  "An include outside the project fails",
			glob:  `glob(name="code", include=["../secret"])`,
			error: `include "../secret" is outside /project`,
		},
		{
			name:  "An include that leaves the project and comes back fails",
			glob:  `glob(name="code", include=["../project/app.sh"])`,
			error: `include "../project/app.sh" is outside /project`,
		},
		{
			name:  "An exclude outside the project fails",
			glob:  `glob(name="code", include=["."], exclude=["../secret"])`,
			error: `exclude "../secret" is outside /project`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newGlobMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.glob)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app")])`)
			w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:5: glob: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestGlob_LimitsArePositive specifies that a selection's limits must be positive.
func TestGlob_LimitsArePositive(t *testing.T) {
	for _, example := range []struct {
		name  string
		glob  string
		error string // after the glob's position
	}{
		{
			name:  "A max_file_count of zero fails",
			glob:  `glob(name="code", include=["."], max_file_count=0)`,
			error: "max_file_count must be positive, not 0",
		},
		{
			name:  "A negative max_file_size fails",
			glob:  `glob(name="code", include=["."], max_file_size=-1)`,
			error: "max_file_size must be positive, not -1",
		},
		{
			name:  "A max_total_size of zero fails",
			glob:  `glob(name="code", include=["."], max_total_size=0)`,
			error: "max_total_size must be positive, not 0",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newGlobMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.glob)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app")])`)
			w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:5: glob: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestGlob_ModeMaskIsWithinThePermissionBits specifies that mode_mask must be between 0 and 0o7777.
func TestGlob_ModeMaskIsWithinThePermissionBits(t *testing.T) {
	for _, example := range []struct {
		name     string
		modeMask string
		error    string // after the glob's position
	}{
		{
			name:     "A mask above 0o7777 fails",
			modeMask: "0o10000",
			error:    "mode_mask 0o10000 must be between 0 and 0o7777",
		},
		{
			name:     "A negative mask fails",
			modeMask: "-1",
			error:    "mode_mask -0o1 must be between 0 and 0o7777",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newGlobMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`glob(name="code", include=["."], mode_mask=` + example.modeMask + `)`)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app")])`)
			w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
			w.WhenISave("app")
			w.ThenItFailsWith("/project/BUILD:2:5: glob: " + example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}
