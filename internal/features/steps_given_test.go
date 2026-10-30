package features

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// Given steps set up a scenario's project.

// GivenAProjectThatPullsBusybox starts the BUILD file with the pull "busybox", of busybox:1.36,
// the one BUILD text that a step writes rather than the scenario. It stops the scenario if the BUILD file holds
// anything already.
func (w *world) GivenAProjectThatPullsBusybox() {
	w.t.Helper()
	if w.files["BUILD"] != "" {
		w.t.Fatal("the BUILD file holds declarations already, which the pull would replace")
	}
	w.files["BUILD"] = testenv.PullBusybox
}

// GivenTheBuildFileDeclares appends build to the BUILD file, dedented.
func (w *world) GivenTheBuildFileDeclares(build string) {
	w.t.Helper()
	w.GivenThePackageDeclares(".", build)
}

// GivenTheBuildFileChanges replaces the first old in the BUILD file with new.
// The BUILD text that it changes is the scenario's own, or a fixture declared in the same feature's file,
// so that a reader can find what it changes. It stops the scenario if the BUILD file doesn't hold old.
func (w *world) GivenTheBuildFileChanges(old, new string) {
	w.t.Helper()
	if !strings.Contains(w.files["BUILD"], old) {
		w.t.Fatalf("the BUILD file doesn't hold %q\n%s", old, w.files["BUILD"])
	}
	w.files["BUILD"] = strings.Replace(w.files["BUILD"], old, new, 1)
}

// GivenThePackageDeclares appends build, dedented, to the BUILD file of the package in dir, "." for the project's.
// A blank build appends nothing, though the package then has its BUILD file, empty if it had none.
func (w *world) GivenThePackageDeclares(dir, build string) {
	w.t.Helper()
	file := path.Join(dir, "BUILD")
	if strings.TrimSpace(build) == "" {
		w.files[file] += ""
		return
	}
	w.files[file] += dedent(build)
}

// GivenIWorkIn makes later commands run in dir, within the project, "." for the project itself,
// as -C names it: unqualified names on the command line then refer to its package.
func (w *world) GivenIWorkIn(dir string) {
	w.t.Helper()
	if dir = path.Clean(dir); path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") {
		w.t.Fatalf("%s isn't within the project", dir)
	}
	w.dir = dir
}

// GivenTheProjectHolds adds files to the project, by path, replacing any it held at those paths.
func (w *world) GivenTheProjectHolds(project files) {
	w.t.Helper()
	maps.Copy(w.files, project)
}

// GivenTheProjectModes gives the project's files and directories these modes, rather than 0o644 and 0o755.
func (w *world) GivenTheProjectModes(project modes) {
	w.t.Helper()
	maps.Copy(w.modes, project)
}

// edit is a change to the project file at path, relative to the project, to hold content,
// when a command opens it for the at'th time. Storing a new file opens it twice, to hash it and then to copy it.
type edit struct {
	path, content string
	at            int
}

// GivenTheFileChangesWhileTheBuildReadsIt changes the project file at path to hold content
// between each command's hashing it and copying it, as an editor might while a build runs.
func (w *world) GivenTheFileChangesWhileTheBuildReadsIt(path, content string) {
	w.t.Helper()
	w.edit = &edit{path, content, 2}
}

// GivenTheFileChangesOnceTheBuildHasReadIt changes the project file at path to hold content
// once each command has read it, as an editor might while a build runs: a third open would be a second read.
func (w *world) GivenTheFileChangesOnceTheBuildHasReadIt(path, content string) {
	w.t.Helper()
	w.edit = &edit{path, content, 3}
}

// GivenTheFileIsTouched sets the modification time of the project file at path an hour ahead, its content as it was.
func (w *world) GivenTheFileIsTouched(path string) {
	w.t.Helper()
	w.touched = append(w.touched, path)
}

// GivenNoFileChangesWhileTheBuildReadsIt undoes GivenTheFileChangesWhileTheBuildReadsIt
// and GivenTheFileChangesOnceTheBuildHasReadIt, for the commands that follow.
func (w *world) GivenNoFileChangesWhileTheBuildReadsIt() {
	w.t.Helper()
	w.edit = nil
}

// GivenTheProjectLinks adds symbolic links to the project, by path, to their targets.
func (w *world) GivenTheProjectLinks(project links) {
	w.t.Helper()
	maps.Copy(w.links, project)
}

// GivenTheEnvironmentSets sets the environment variable name of the vamos process, for the rest of the scenario.
func (w *world) GivenTheEnvironmentSets(name, value string) {
	w.t.Helper()
	w.t.Setenv(name, value)
}

// GivenTheEnvironmentDoesNotSet unsets the environment variable name of the vamos process, for the rest of the
// scenario.
func (w *world) GivenTheEnvironmentDoesNotSet(name string) {
	w.t.Helper()
	w.t.Setenv(name, "") // which restores it once the scenario ends
	if err := os.Unsetenv(name); err != nil {
		w.t.Fatal(err)
	}
}

// GivenAFileOutsideTheProjectHolds writes content to the file name in a folder outside the project,
// which <outside> stands for in the secrets of the withSecrets option.
func (w *world) GivenAFileOutsideTheProjectHolds(name, content string) {
	w.t.Helper()
	if w.outside == "" {
		w.outside = w.t.TempDir()
	}
	if err := os.WriteFile(filepath.Join(w.outside, name), []byte(content), 0o600); err != nil {
		w.t.Fatal(err)
	}
}
