package features

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newExportWorld starts a scenario of the export feature, from its background.
// export writes the files of a task declaring source and include to a folder,
// at their paths relative to the task's source.
// Its project pulls busybox.
func newExportWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newExportMistakeWorld starts a scenario of the export feature that fails before anything is built,
// so that it needs no buildah. Its project pulls busybox.
func newExportMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// aTargetOfEachKind declares a target of each kind, the glob "code" of the project's app.sh among them,
// for the export's mistakes to name.
const aTargetOfEachKind = `
	glob(name="code", include=["app.sh"])
	task(name="make", base="busybox", steps=[run(["mkdir", "/out"])], source="/out", include=["."])
	task(name="test", base="busybox", steps=[run(["true"])])
	layer(name="app", base="busybox")
	index(name="all", layer="app", platforms=["linux/amd64", "linux/arm64"])
`

// TestExport_FilesKeepModesLinksAndTheTasksTimestamp specifies that exported files receive the task's timestamp,
// and keep the modes the task selected them with, and that symbolic links are written as links, never followed.
func TestExport_FilesKeepModesLinksAndTheTasksTimestamp(t *testing.T) {
	t.Run("Files keep their modes and links, and take the task's time", func(t *testing.T) {
		w := newExportWorld(t)
		nonce := testenv.NewNonce()
		stamp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) // after busybox was made
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			task(
			    name="make",
			    base="busybox",
			    steps=[run(["sh", "-c", """
			        mkdir -p /out/sub
			        echo %s > /out/a.txt
			        echo b > /out/sub/b.txt
			        chmod 750 /out/sub
			        chmod 640 /out/a.txt
			        ln -s a.txt /out/link
			        ln -s /etc/passwd /out/passwd
			    """])],
			    source="/out",
			    include=["."],
			)
		`, nonce))
		w.WhenIExport("make", stampedAt(stamp.Format(time.RFC3339)))
		w.ThenItSucceeds()
		w.ThenItLastReports("Files exported! <destination>/make sha256:")
		w.ThenTheExportPathsAre("a.txt", "sub", "sub/b.txt", "link", "passwd")
		w.ThenTheExportHoldsAtLeast(files{"a.txt": nonce + "\n", "sub/b.txt": "b\n"})
		w.ThenTheExportLinks(links{"link": "a.txt", "passwd": "/etc/passwd"})
		w.ThenTheExportModesAre(modes{"a.txt": 0o640, "sub": 0o750})
		w.ThenTheExportFilesWereModifiedAt(stamp, ".", "a.txt", "sub", "sub/b.txt", "link", "passwd")
	})

	t.Run("Links are written as they are, at the task's time, and what they lead to is left as it was",
		func(t *testing.T) {
			w := newExportWorld(t)
			stamp := time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) // after 2262, which nanoseconds since 1970 can't hold
			w.GivenAFileOutsideTheProjectHolds("victim", "keep")
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				task(
				    name="make",
				    base="busybox",
				    steps=[run(["sh", "-c", """
				        mkdir /out && echo %s > /out/a.txt
				        ln -s '<outside>/victim' /out/abs
				        ln -s '../../../../../../../../..<outside>/victim' /out/up
				        ln -s /no/such/file /out/dangling
				    """])],
				    source="/out",
				    include=["."],
				)
			`, testenv.NewNonce()))
			w.WhenIExport("make", stampedAt(stamp.Format(time.RFC3339)))
			w.ThenItSucceeds()
			w.ThenTheExportPathsAre("a.txt", "abs", "up", "dangling")
			w.ThenTheExportLinks(links{
				"abs":      "<outside>/victim",
				"up":       "../../../../../../../../..<outside>/victim",
				"dangling": "/no/such/file",
			})
			w.ThenTheExportFilesWereModifiedAt(stamp, ".", "a.txt", "abs", "up", "dangling")
			w.ThenTheFileOutsideTheProjectIsAsItWas("victim")
			w.ThenTheDestinationPathsAre("make", "make/a.txt", "make/abs", "make/up", "make/dangling") // no staging
		})
}

// TestExport_FolderIsReplacedAsAWhole specifies that the folder is replaced as a whole,
// rather than merged with existing contents, and that a nonempty folder requires --replace.
func TestExport_FolderIsReplacedAsAWhole(t *testing.T) {
	t.Run("A folder that holds files is replaced only with --replace, losing the files the task doesn't make",
		func(t *testing.T) {
			w := newExportWorld(t)
			w.GivenTheBuildFileDeclares(`
				task(
				    name="make",
				    base="busybox",
				    steps=[run(["sh", "-c", "mkdir /out && echo a > /out/a.txt && ln -s a.txt /out/link"])],
				    source="/out",
				    include=["."],
				)
			`)
			w.WhenIExport("make")
			w.ThenItSucceeds()
			w.GivenTheNextCommandSendsWhereTheLastDid()
			w.GivenTheDestinationHolds(files{"make/stale.txt": "old"})
			w.WhenIExport("make")
			w.ThenItFailsWith("exporting to <destination>/make: it isn't empty; pass --replace to replace it")
			w.ThenTheDestinationPathsAre("make", "make/a.txt", "make/link", "make/stale.txt")
			w.GivenTheNextCommandSendsWhereTheLastDid()
			w.WhenIExport("make", replacing())
			w.ThenItSucceeds()
			w.ThenTheExportPathsAre("a.txt", "link")
			w.ThenTheExportHoldsAtLeast(files{"a.txt": "a\n"})
			w.ThenTheExportLinks(links{"link": "a.txt"})
			w.ThenTheDestinationPathsAre("make", "make/a.txt", "make/link")
		})

	t.Run("An empty folder is filled without --replace", func(t *testing.T) {
		w := newExportWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(name="make", base="busybox", steps=[run(["sh", "-c", "mkdir /out && echo a > /out/a.txt"])],
			     source="/out", include=["."])
		`)
		w.GivenTheDestinationHasTheFolders("make")
		w.WhenIExport("make")
		w.ThenItSucceeds()
		w.ThenTheExportHolds(files{"a.txt": "a\n"})
		w.ThenTheDestinationPathsAre("make", "make/a.txt") // and no staging folder
	})

	t.Run("A failing task leaves the folder as it was", func(t *testing.T) {
		w := newExportWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(name="make", base="busybox", steps=[run(["false"])], source="/out", include=["."])
		`)
		w.GivenTheDestinationHolds(files{"make/old.txt": "old"})
		w.WhenIExport("make", replacing())
		w.ThenItFailsWith(`task "make": run(["false"]) failed`)
		w.ThenTheDestinationHolds(files{"make/old.txt": "old"})
		w.ThenNoImageIsLeftBehind()
	})
}

// TestExport_AsRootFilesTakeTheParentsOwner specifies that when vamos runs as root, exported files receive the owner
// and group of the destination's parent directory, keeping their setuid and setgid bits, which changing an owner
// clears, where the task's mode_mask retains them.
func TestExport_AsRootFilesTakeTheParentsOwner(t *testing.T) {
	t.Run("Every file, folder and link exported takes the parent's owner, and keeps its special bits",
		func(t *testing.T) {
			w := newExportWorld(t)
			w.GivenAFileOutsideTheProjectHolds("victim", "keep")
			w.GivenTheDestinationIsOwnedBy("1234:1235")
			w.GivenTheBuildFileDeclares(fmt.Sprintf(`
				task(
				    name="make",
				    base="busybox",
				    steps=[run(["sh", "-c", """
				        mkdir -p /out/deep/x && echo %s > /out/a.txt && echo y > /out/deep/x/y.txt
				        ln -s a.txt /out/link
				        ln -s '<outside>/victim' /out/abs
				        ln -s '../../../../../../../../..<outside>/victim' /out/up
				        ln -s /no/such/file /out/dangling
				        echo s > /out/setuid && chmod 4755 /out/setuid
				        echo g > /out/setgid && chmod 2755 /out/setgid
				    """])],
				    source="/out",
				    # deep and deep/x, which no pattern selects, are made to hold deep/x/y.txt
				    include=["[^d]*", "dang*", "deep/x/*"],
				    mode_mask=0o7777,
				)
			`, testenv.NewNonce()))
			w.WhenIExport("make")
			w.ThenItSucceeds()
			w.ThenTheExportOwnersAre(owners{
				".":            "1234:1235",
				"a.txt":        "1234:1235",
				"deep":         "1234:1235",
				"deep/x":       "1234:1235",
				"deep/x/y.txt": "1234:1235",
				"link":         "1234:1235",
				"abs":          "1234:1235",
				"up":           "1234:1235",
				"dangling":     "1234:1235",
				"setuid":       "1234:1235",
				"setgid":       "1234:1235",
			})
			w.ThenTheExportModesAre(modes{"setuid": 0o4755, "setgid": 0o2755})
			w.ThenTheFileOutsideTheProjectIsAsItWas("victim")
			w.ThenTheDestinationPathsAre("make", "make/a.txt", "make/deep", "make/deep/x", "make/deep/x/y.txt",
				"make/link", "make/abs", "make/up", "make/dangling", "make/setuid", "make/setgid") // and no staging
		})

}

// TestExport_RequiresATaskWithOutputAndAFolderToWrite specifies that export requires a task declaring source and
// include, and a folder that is empty or replaced, whose parent directory already exists.
func TestExport_RequiresATaskWithOutputAndAFolderToWrite(t *testing.T) {
	for _, example := range []struct {
		name    string
		target  string
		options []option
		holds   files    // the files that the destination holds, before and after
		folders []string // the empty folders that the destination holds, before and after
		links   links    // the links that the destination holds, before and after
		outside files    // the files outside the project, which links may lead to, as they were after
		error   string
	}{
		{
			name:   "A layer can't be exported",
			target: "app",
			error:  `"app" is a layer(), not a task(); export writes a task's files, and "vamos save" writes an image`,
		},
		{
			name:   "A pull can't be exported",
			target: "busybox",
			error:  `"busybox" is a pull(), not a task(); export writes a task's files`,
		},
		{
			name:   "An index can't be exported",
			target: "all",
			error:  `"all" is an index(), not a task(); export writes a task's files`,
		},
		{
			name:   "A glob can't be exported",
			target: "code",
			error:  `"code" is a glob(), whose files are already in the project; export writes a task's files`,
		},
		{
			name:   "A task without source can't be exported",
			target: "test",
			error:  `"test" is a task() without source, so it has no files to export; "vamos build test" runs it`,
		},
		{
			name:   "A folder that holds files isn't replaced without --replace",
			target: "make",
			holds:  files{"make/kept.txt": "kept"},
			error:  "exporting to <destination>/make: it isn't empty; pass --replace to replace it",
		},
		{
			name:    "A folder that holds only a folder isn't replaced without --replace",
			target:  "make",
			folders: []string{"make/old"},
			error:   "exporting to <destination>/make: it isn't empty; pass --replace to replace it",
		},
		{
			name:    "A file where the folder would be fails",
			target:  "make",
			options: []option{replacing()},
			holds:   files{"make": "a file"},
			error:   "exporting to <destination>/make: it isn't a directory",
		},
		{
			name:    "A link where the folder would be fails, even with --replace",
			target:  "make",
			options: []option{replacing()},
			links:   links{"make": "<outside>/folder"},
			outside: files{"folder/kept.txt": "kept"},
			error:   "exporting to <destination>/make: it isn't a directory",
		},
		{
			name:    "A missing parent fails",
			target:  "make",
			options: []option{to("nope/make")},
			error:   "exporting to <destination>/nope/make: directory <destination>/nope does not exist",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newExportMistakeWorld(t)
			w.GivenTheBuildFileDeclares(aTargetOfEachKind)
			w.GivenTheProjectHolds(files{"app.sh": ""})
			w.GivenTheDestinationHolds(example.holds)
			w.GivenTheDestinationHasTheFolders(example.folders...)
			w.GivenFilesOutsideTheProjectHold(example.outside)
			w.GivenTheDestinationLinks(example.links)
			w.WhenIExport(example.target, example.options...)
			w.ThenItFailsWith(example.error)
			w.ThenTheDestinationPathsAre(pathsOf(example.holds, append(example.folders,
				slices.Collect(maps.Keys(example.links))...))...)
			w.ThenTheDestinationHoldsAtLeast(example.holds)
			w.ThenTheDestinationLinksAtLeast(example.links)
			w.ThenTheFilesOutsideTheProjectAreAsTheyWere()
			w.ThenNothingElseIsOutsideTheProject()
		})
	}
}
