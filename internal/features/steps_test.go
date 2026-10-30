package features

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cli"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/afero"
)

// The steps are split by subject, in files that sort together:
// this one holds the world, how it runs commands, and the comparisons that its Then steps share;
// steps_given_test.go the project's Given steps; steps_commands_test.go the When steps and their options;
// steps_output_test.go what a command reported, its targets' statuses and its steps' observations;
// steps_image_test.go the image saved;
// and steps_destination_test.go the folder a command sends to, and the files exported there.

// files are regular files' contents, by path: what a project holds, or what to expect of an image or a folder.
type files map[string]string

// modes are the permission and special bits of files and directories, by path:
// what a project gives its files, or what to expect of an image or a folder.
type modes map[string]int64

// links are symbolic links' targets, by path: what a project holds, or what to expect of an image.
type links map[string]string

// owners are the owners of files and directories in an image, by path, as "uid:gid".
type owners map[string]string

// world is the state of one scenario: the project it declares, and what the last command it ran did.
// Its methods named Given, When or Then are the steps that scenarios are written in.
type world struct {
	t       *testing.T
	stores  *testenv.Stores
	files   files           // the project's files, BUILD among them
	modes   modes           // the modes of the project's files and directories, where not the default
	links   links           // the project's symbolic links, which keep it on disk rather than in memory
	edit    *edit           // a change to a project file once a command has read it, if any
	touched []string        // the project files whose modification times are an hour ahead
	denied  []string        // the project files that can't be checked
	dir     string          // the directory within the project that commands run in, with -C
	cDir    string          // the directory that -C names as given, rather than dir, if a step says
	onDisk  bool            // whether the project is kept on disk, under root, even without links
	root    string          // the folder that holds the project, if it is on disk
	before  map[string]bool // the store's unnamed images when the scenario began, if it builds

	prepared string // the folder that the next command sends to, if Given steps have put something there
	outside  string // a folder outside the project, for files that commands read, such as secrets

	stdout    string
	stderr    string
	err       error            // the last command's
	dest      string           // the folder that the last command sent to, its own, if it sends
	inProject bool             // whether the last command saved into the project, whose other files it didn't send
	archive   string           // the archive that the last command saved, if it saved one
	sent      string           // what the last command reports once it has sent its image or files, if it sends any
	image     v1.Image         // the image that the last command saved, if it saved one
	first     v1.Image         // the first image that the scenario saved
	entries   map[string]entry // the image's files, once a step has read them
	exported  string           // the folder that the last command exported to, if it exported
}

// newBuildingWorld starts a scenario that builds, which it skips where buildah can't,
// noting the store's images so that ThenNoImageIsLeftBehind can tell those the scenario leaves.
func newBuildingWorld(t *testing.T) *world {
	t.Helper()
	testenv.RequireBuildah(t)
	w := newWorld(t)
	before, err := w.stores.UnnamedImages()
	if err != nil {
		t.Fatal(err)
	}
	w.before = before
	return w
}

// newWorld starts a scenario that needs no buildah, since it fails before anything is built.
func newWorld(t *testing.T) *world {
	t.Helper()
	return &world{t: t, stores: testenv.SuiteStores(t), files: files{}, modes: modes{}, links: links{}}
}

// run runs the vamos command line with args, in the scenario's project, as main does.
// It forgets what earlier commands saved and exported, so that checks see only this one's.
func (w *world) run(args ...string) {
	w.t.Helper()
	w.runIn(w.project(), args...)
}

// runIn is run in the project fsys. -C comes first, before the command, so that args end as a scenario gives them,
// even with a flag that lacks its value.
func (w *world) runIn(fsys afero.Fs, args ...string) {
	w.t.Helper()
	dir := w.cDir
	if dir == "" {
		dir = filepath.Join(testenv.ProjectDir, w.dir)
		if info, err := fsys.Stat(dir); err != nil || !info.IsDir() {
			w.t.Fatalf("the project has no directory %s to work in", w.dir)
		}
	}
	w.execute(fsys, append([]string{"-C", dir}, args...))
}

// execute runs the vamos command line with exactly args, none for none, in the project fsys, as main does.
func (w *world) execute(fsys afero.Fs, args []string) {
	w.t.Helper()
	w.image, w.archive, w.entries, w.exported = nil, "", nil, ""
	cmd := cli.NewRootCommand(cli.Config{Fs: fsys, Store: &w.stores.Options})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{}, args...)) // never nil, which would make Cobra read the test's own arguments
	w.err = cmd.ExecuteContext(w.t.Context())
	w.stdout, w.stderr = stdout.String(), stderr.String()
}

// project returns a new filesystem holding the scenario's project: in memory, unless it holds links
// or a step keeps it on disk.
func (w *world) project() afero.Fs {
	w.t.Helper()
	var fsys afero.Fs
	if len(w.links) > 0 || w.onDisk {
		links := links{}
		for path, target := range w.links {
			links[path] = w.atOutside(target)
		}
		fsys, w.root = testenv.DiskProject(w.t, w.files, links)
	} else {
		fsys = testenv.Project(w.t, w.files)
	}
	for path, mode := range w.modes {
		w.notThroughLink(path)
		if err := fsys.Chmod(filepath.Join(testenv.ProjectDir, path), fs.FileMode(mode)); err != nil {
			w.t.Fatal(err)
		}
	}
	for _, path := range w.touched {
		w.notThroughLink(path)
		later := time.Now().Add(time.Hour)
		if err := fsys.Chtimes(filepath.Join(testenv.ProjectDir, path), later, later); err != nil {
			w.t.Fatal(err)
		}
	}
	if w.edit != nil {
		path := filepath.Join(testenv.ProjectDir, w.edit.path)
		fsys = &testenv.EditedFs{Fs: fsys, File: path, Edit: w.edit.content, At: w.edit.at}
	}
	if len(w.denied) > 0 {
		denied := &testenv.DeniedFs{Fs: fsys}
		for _, path := range w.denied {
			denied.Files = append(denied.Files, filepath.Join(testenv.ProjectDir, path))
		}
		fsys = denied
	}
	return fsys
}

// atOutside returns s with each <outside> replaced by the folder of GivenAFileOutsideTheProjectHolds,
// failing the scenario if s names it but no file outside the project was given.
func (w *world) atOutside(s string) string {
	w.t.Helper()
	if strings.Contains(s, "<outside>") && w.outside == "" {
		w.t.Fatalf("%q names <outside>, but no file outside the project was given", s)
	}
	return strings.ReplaceAll(s, "<outside>", w.outside)
}

// notThroughLink fails the scenario if the project path is a link or beneath one:
// Chmod and Chtimes follow links, so they would change what a link points to, perhaps on the host.
func (w *world) notThroughLink(path string) {
	w.t.Helper()
	for link := range w.links {
		path, link := filepath.Clean(path), filepath.Clean(link)
		if path == link || strings.HasPrefix(path, link+"/") {
			w.t.Fatalf("%s is reached through the symbolic link %s", path, link)
		}
	}
}

// check reports err, which may join several, as the scenario's failure.
func (w *world) check(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Error(err)
	}
}

// entry is a file, directory or other node, such as a link, in an image or a folder.
type entry struct {
	dir     bool
	regular bool      // a regular file
	symlink bool      // a symbolic link
	mode    int64     // permission and special bits
	content string    // a regular file's
	target  string    // a symbolic link's
	modTime time.Time // when it was last modified
	owner   string    // its owner and group in an image, as "uid:gid"
}

// matchFiles checks entries against want, regular files by path.
// Exactly, want must list every file in entries, and every directory in entries must hold one of them:
// a link, another file or an empty directory is there too.
func matchFiles(entries map[string]entry, want files, exactly bool) error {
	var errs []error
	for _, path := range slices.Sorted(maps.Keys(want)) {
		e, ok := entries[path]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s not found", path))
		case e.dir:
			errs = append(errs, fmt.Errorf("%s is a directory", path))
		case !e.regular:
			errs = append(errs, fmt.Errorf("%s is not a regular file", path))
		case e.content != want[path]:
			errs = append(errs, fmt.Errorf("%s = %q, want %q", path, e.content, want[path]))
		}
	}
	if exactly {
		errs = append(errs, unlisted(entries, want, func(path string, e entry) bool {
			return !e.dir || !holdsAny(path, want)
		})...)
	}
	return errors.Join(errs...)
}

// unlisted returns an error for each of entries that want doesn't list and that listed reports belongs in the listing.
func unlisted[V any](entries map[string]entry, want map[string]V, listed func(path string, e entry) bool) []error {
	var errs []error
	for _, path := range slices.Sorted(maps.Keys(entries)) {
		if _, ok := want[path]; !ok && listed(path, entries[path]) {
			errs = append(errs, fmt.Errorf("%s is there too", path))
		}
	}
	return errs
}

// matchPaths checks that the paths of entries, directories among them, are exactly want.
func matchPaths(entries map[string]entry, want []string) error {
	got, want := slices.Sorted(maps.Keys(entries)), slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		return fmt.Errorf("the paths are %q, want %q", got, want)
	}
	return nil
}

// matchLinks checks that entries holds want's symbolic links, by path, to their targets.
// Exactly, want must list every symbolic link in entries.
func matchLinks(entries map[string]entry, want links, exactly bool) error {
	var errs []error
	if exactly {
		errs = unlisted(entries, want, func(_ string, e entry) bool { return e.symlink })
	}
	for _, path := range slices.Sorted(maps.Keys(want)) {
		if e, ok := entries[path]; !ok {
			errs = append(errs, fmt.Errorf("no symbolic link %s", path))
		} else if !e.symlink {
			errs = append(errs, fmt.Errorf("%s is there, but not as a symbolic link", path))
		} else if e.target != want[path] {
			errs = append(errs, fmt.Errorf("%s links to %q, want %q", path, e.target, want[path]))
		}
	}
	return errors.Join(errs...)
}

// matchModTimes checks that the entries at paths were last modified at want.
func matchModTimes(entries map[string]entry, want time.Time, paths []string) error {
	var errs []error
	for _, path := range paths {
		if e, ok := entries[path]; !ok {
			errs = append(errs, fmt.Errorf("%s not found", path))
		} else if !e.modTime.Equal(want) {
			errs = append(errs, fmt.Errorf("%s modified at %v, want %v", path, e.modTime, want))
		}
	}
	return errors.Join(errs...)
}

// matchModes checks the modes of entries, files or directories, against want.
func matchModes(entries map[string]entry, want modes) error {
	var errs []error
	for _, path := range slices.Sorted(maps.Keys(want)) {
		if e, ok := entries[path]; !ok {
			errs = append(errs, fmt.Errorf("%s not found", path))
		} else if e.mode != want[path] {
			errs = append(errs, fmt.Errorf("mode of %s = %#o, want %#o", path, e.mode, want[path]))
		}
	}
	return errors.Join(errs...)
}

// matchOwners checks the owners of entries, files or directories, against want.
func matchOwners(entries map[string]entry, want owners) error {
	var errs []error
	for _, path := range slices.Sorted(maps.Keys(want)) {
		if e, ok := entries[path]; !ok {
			errs = append(errs, fmt.Errorf("%s not found", path))
		} else if e.owner != want[path] {
			errs = append(errs, fmt.Errorf("owner of %s = %s, want %s", path, e.owner, want[path]))
		}
	}
	return errors.Join(errs...)
}

// holdsAny reports whether directory dir holds any of want's paths.
func holdsAny[V any](dir string, want map[string]V) bool {
	for path := range want {
		if isBeneath(path, dir) {
			return true
		}
	}
	return false
}

// beneath returns the entries whose paths are beneath dir.
func beneath(entries map[string]entry, dir string) map[string]entry {
	within := map[string]entry{}
	for path, e := range entries {
		if isBeneath(path, dir) {
			within[path] = e
		}
	}
	return within
}

// isBeneath reports whether path is beneath dir, rather than dir itself or beside it.
func isBeneath(path, dir string) bool {
	return strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}
