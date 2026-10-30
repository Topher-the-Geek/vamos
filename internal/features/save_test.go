package features

import (
	"syscall"
	"testing"
)

// newSaveWorld starts a scenario of the save feature, from its background.
// save writes an archive of an image that docker load can import.
// The project pulls busybox and declares theApp, with the hello.txt that it adds.
func newSaveWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	w.GivenTheBuildFileDeclares(theApp)
	w.GivenTheProjectHolds(files{"hello.txt": "hello, file\n"})
	return w
}

// newSaveMistakeWorld starts a scenario of the save feature that fails before anything is built,
// so that it needs no buildah. Its project pulls busybox.
func newSaveMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// theApp declares the save feature's layer "app", which adds the project's hello.txt to busybox at /data
// and echoes a greeting.
const theApp = `
	glob(name="hello", include=["hello.txt"])
	layer(name="app", base="busybox", steps=[add("hello", "/data")], command=["echo", "hello from vamos"])
`

// TestSave_ArchiveNamesTheTaggedImage specifies that --tag records an image name in the archive,
// and that without it, the archive has no image name.
func TestSave_ArchiveNamesTheTaggedImage(t *testing.T) {
	t.Run("A tagged archive names its image", func(t *testing.T) {
		w := newSaveWorld(t)
		w.WhenISave("app", tagged("app:test"))
		w.ThenItSucceeds()
		w.ThenItLastReports("Image built! <destination>/app.tar ")
		w.ThenTheArchiveNames("docker.io/library/app:test") // as docker load shows app:test
		w.ThenTheImageCommandIs("echo", "hello from vamos")
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": "hello, file\n"})
	})

	t.Run("An untagged archive names no image", func(t *testing.T) {
		w := newSaveWorld(t)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheArchiveNames()
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": "hello, file\n"})
	})
}

// TestSave_WritesAnArchive specifies that save writes an archive that docker load can import, at any path,
// colons and all, even inside the project, whose globs never read the archive being written.
func TestSave_WritesAnArchive(t *testing.T) {
	for _, example := range []struct {
		name    string
		options []option
	}{
		{
			name:    "An archive's path may hold colons",
			options: []option{to("builds:2026/app:1.tar")},
		},
		{
			name:    "An archive's path may hold colons, with a tag",
			options: []option{to("builds:2026/app:1.tar"), tagged("app:test")},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newSaveWorld(t)
			w.GivenTheDestinationHasTheFolders("builds:2026")
			w.WhenISave("app", example.options...)
			w.ThenItSucceeds()
			w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": "hello, file\n"})
			w.ThenTheDestinationPathsAre("builds:2026", "builds:2026/app:1.tar")
		})
	}

	t.Run("An archive saved into the project isn't read as a source of its own image", func(t *testing.T) {
		w := newSaveWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="project", include=["."])
			layer(name="all", base="busybox", steps=[add("project", "/app")])
		`)
		w.WhenISave("all", intoTheProject("all.tar"))
		w.ThenItSucceeds()
		w.ThenTheImagePathsAre(under("/app"), "/app/BUILD", "/app/hello.txt")
	})

	t.Run("A new archive is readable by all, as the umask allows", func(t *testing.T) {
		// The umask is the process's, so this would be unsafe for scenarios in parallel, which they aren't.
		umask := syscall.Umask(0o022)
		t.Cleanup(func() { syscall.Umask(umask) })
		w := newSaveWorld(t)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheDestinationModesAre(modes{"app.tar": 0o644})
	})
}

// TestSave_WriteIsADeprecatedName specifies that write is a deprecated earlier name for save
// and still accepts the same arguments.
func TestSave_WriteIsADeprecatedName(t *testing.T) {
	t.Run("write saves, and says it is deprecated", func(t *testing.T) {
		w := newSaveWorld(t)
		w.WhenIWrite("app")
		w.ThenItSucceeds()
		w.ThenTheCommandSays(`Command "write" is deprecated, use "vamos save"`)
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": "hello, file\n"})
	})
}

// TestSave_ExistingFileIsReplacedOnceTheImageIsComplete specifies that an existing output file is replaced
// only after the new image is complete.
func TestSave_ExistingFileIsReplacedOnceTheImageIsComplete(t *testing.T) {
	t.Run("An existing file is replaced, keeping its mode", func(t *testing.T) {
		w := newSaveWorld(t)
		w.GivenTheDestinationHolds(files{"app.tar": "an old image"})
		w.GivenTheDestinationModes(modes{"app.tar": 0o640})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": "hello, file\n"})
		w.ThenTheDestinationPathsAre("app.tar")
		w.ThenTheDestinationModesAre(modes{"app.tar": 0o640})
	})

	t.Run("A failed build leaves an existing file as it was", func(t *testing.T) {
		w := newSaveWorld(t)
		w.GivenTheBuildFileDeclares(`layer(name="broken", base="busybox", steps=[run(["false"])])`)
		w.GivenTheDestinationHolds(files{"broken.tar": "an old image"})
		w.WhenISave("broken")
		w.ThenItFailsWith(`run(["false"]) failed`)
		w.ThenTheDestinationHolds(files{"broken.tar": "an old image"})
	})
}

// TestSave_DestinationMustBeAFileInAFolder specifies that the output file's parent directory must already exist,
// and that the output path must not be a directory.
func TestSave_DestinationMustBeAFileInAFolder(t *testing.T) {
	for _, example := range []struct {
		name    string
		holds   files    // the files that the destination holds, before and after
		folders []string // the empty folders that the destination holds, before and after
		path    string   // where to save, relative to the destination
		error   string
	}{
		{
			name:  "A missing folder fails",
			path:  "nope/app.tar",
			error: "cannot write <destination>/nope/app.tar: directory <destination>/nope does not exist",
		},
		{
			name:    "A folder where the file would be fails",
			folders: []string{"app.tar"},
			path:    "app.tar",
			error:   "cannot write <destination>/app.tar: it is a directory",
		},
		{
			name:  "A file where the folder would be fails",
			holds: files{"file": ""},
			path:  "file/app.tar",
			error: "<destination>/file is not a directory",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newSaveMistakeWorld(t)
			w.GivenTheBuildFileDeclares(theApp)
			w.GivenTheProjectHolds(files{"hello.txt": "hello, file\n"})
			w.GivenTheDestinationHolds(example.holds)
			w.GivenTheDestinationHasTheFolders(example.folders...)
			w.WhenISave("app", to(example.path))
			w.ThenItFailsWith(example.error)
			w.ThenTheDestinationPathsAre(pathsOf(example.holds, example.folders)...)
			w.ThenTheDestinationHoldsAtLeast(example.holds)
		})
	}
}

// TestSave_TasksImagesAreDiscarded specifies that only a task's selected output survives its temporary container,
// so it has no image to save.
func TestSave_TasksImagesAreDiscarded(t *testing.T) {
	t.Run("A task can't be saved", func(t *testing.T) {
		w := newSaveMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			task(name="make", base="busybox", steps=[run(["mkdir", "/out"])], source="/out", include=["."])
		`)
		w.WhenISave("make")
		w.ThenItFailsWith(`"make" is a task(), whose image is discarded once it runs; ` +
			`name a pull(), layer() or index() to send, or export the task's files with "vamos export"`)
		w.ThenNothingIsSaved()
	})
}
