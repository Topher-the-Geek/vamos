package features

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cli"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/afero"
	"go.podman.io/storage"
)

// The steps are split by subject, in files that sort together:
// this one holds the world, how it runs commands, and the comparisons that its Then steps share;
// steps_given_test.go the project's Given steps; steps_commands_test.go the When steps and their options;
// steps_output_test.go what a command reported, its targets' statuses and its steps' observations;
// steps_image_test.go the image saved;
// steps_destination_test.go the folder a command sends to, and the files exported there;
// steps_trace_test.go the trace that --trace writes; steps_interrupt_test.go interrupting a command;
// steps_remote_test.go coordinators that a command builds on;
// and steps_cache_test.go the build cache and the container store as other programs see them.

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
	linked  bool            // whether the project's directory is a symbolic link to project-real beside it
	root    string          // the folder that holds the project, if it is on disk
	before  map[string]bool // the store's unnamed images when the scenario began, if it builds

	prepared    string                 // the folder the next command sends to, if Given steps have put something there
	outside     string                 // a folder outside the project, for files that commands read, such as secrets
	outsideWas  map[string]outsideFile // what GivenAFileOutsideTheProjectHolds wrote there, by name
	registry    string                 // the host of the scenario's registry, <registry>, once a Given step starts it
	coordinator string                 // the URL of the scenario's coordinator, <coordinator>, once started
	jobs        atomic.Int64           // the jobs that the scenario's coordinator was given
	served      atomic.Int64           // the requests that the scenario's coordinator was sent
	login       authn.Authenticator    // that the scenario's registry needs, which steps that read from it use
	server      *httptest.Server       // that serves the scenario's registry, once a Given step starts it
	asked       atomic.Int64           // the requests that the scenario's registry has served
	stopped     atomic.Bool            // whether the scenario's registry has stopped answering
	held        map[string]v1.Image    // the images that Given steps put in the registry, by label and platform

	stdout       string
	stderr       string
	err          error             // the last command's
	dest         string            // the folder that the last command sent to, its own, if it sends
	inProject    bool              // whether the last command saved into the project, whose other files it didn't send
	archive      string            // the archive that the last command saved, if it saved one
	sent         string            // what the last command reports once it has sent its image or files, if it sends any
	image        v1.Image          // the image that the last command saved or pushed, if it sent one
	index        v1.ImageIndex     // the index that the last command pushed, if it pushed one
	sentAs       string            // the image name that the last command pushed or loaded as, if it pushed or loaded
	docker       func() [][]byte   // the images that the scenario's own Docker daemon has loaded, if it has one
	daemon       string            // which Docker daemon a Given step gave: "fake", "none" or "host"
	dockerHost   string            // the host's Docker daemon, as vamos finds it, if the scenario loads into it
	loadedBefore int               // how many images the scenario's own daemon had loaded before the last command
	loadRan      bool              // whether the last command loaded into Docker, or tried to
	traceTo      string            // where the command running is to write its trace, if traced() asked it to
	traceAt      string            // where tracedTo asked the command running to write its trace, with placeholders
	interruptOn  string            // what the command running prints that an option or step interrupts it on
	interruptIn  time.Duration     // how long interruptedAfter lets the command running run
	interruptBy  time.Duration     // how long a step lets the command running run, if what it waits for never comes
	printedCue   bool              // whether the last command printed what it was interrupted on, before any deadline
	containers   map[string]bool   // the containers in the store when the scenario started, if it builds
	took         time.Duration     // how long the last command ran
	outputClosed bool              // whether the command running's standard output fails every write
	tried        string            // what the last command tried to print to standard output that was closed
	tracePath    string            // where the last command wrote its trace, if traced() asked it to
	trace        *trace            // the trace there, once a step has read it
	graph        *graph.Graph      // the graph that WhenIShowTheGraph showed last
	ran          bool              // whether any command has run
	ownStores    bool              // whether the scenario's stores are its own, rather than the suite's
	other        func()            // releases what another vamos holds of the build cache, if one does
	suiteBefore  cached            // what the suite's stores held once GivenTheSuitesStoresHoldAResult built there
	alsoNamed    map[string]string // the images that GivenACachedLayerIsAlsoNamed named, by name
	cachedBefore cached            // what its own stores held as the last command began, alone
	cachedAfter  cached            // and as it ended
	emulating    string            // the platform whose programs the scenario runs under emulation, if any
	askedDuring  int64             // the requests that the scenario's registry served as the last command ran
	jobsDuring   int64             // the jobs that the scenario's coordinator was given as the last command ran
	servedDuring int64             // the requests that the scenario's coordinator was sent as the last command ran
	first        v1.Image          // the first image that the scenario saved
	saved        []v1.Image        // the images that the scenario saved, in order
	entries      map[string]entry  // the image's files, once a step has read them
	exported     string            // the folder that the last command exported to, if it exported
}

// newBuildingWorld starts a scenario that builds, which it skips where buildah can't, as GivenBuildahCanBuildHere.
func newBuildingWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenBuildahCanBuildHere()
	return w
}

// GivenBuildahCanBuildHere skips the scenario where buildah can't build, since it builds, and notes the store's
// images and containers, so that ThenNoImageIsLeftBehind and ThenTheCommandLeavesNoContainer can tell those the
// scenario leaves.
func (w *world) GivenBuildahCanBuildHere() {
	w.t.Helper()
	if w.ran {
		w.t.Fatal("GivenBuildahCanBuildHere comes before any command, so that it notes what the scenario leaves")
	}
	testenv.RequireBuildah(w.t)
	err := w.stores.WithStore(func(store storage.Store) error {
		images, err := store.Images()
		if err != nil {
			return err
		}
		w.before = map[string]bool{}
		for _, img := range images {
			if len(img.Names) == 0 {
				w.before[img.ID] = true
			}
		}
		containers, err := store.Containers()
		w.containers = map[string]bool{}
		for _, c := range containers {
			w.containers[c.ID] = true
		}
		return err
	})
	if err != nil {
		w.t.Fatal(err)
	}
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
	if w.traceAt != "" { // now that the When step has made the destination it may name
		args = append(slices.Clone(args), "--trace", w.atPlaceholders(w.traceAt))
		w.traceAt = ""
	}
	w.execute(fsys, append([]string{"-C", dir}, args...))
}

// execute runs the vamos command line with exactly args, none for none, in the project fsys, as main does.
func (w *world) execute(fsys afero.Fs, args []string) {
	w.t.Helper()
	w.image, w.index, w.archive, w.entries, w.exported, w.sentAs = nil, nil, "", nil, "", ""
	w.loadRan, w.trace = false, nil
	w.tracePath, w.traceTo = w.traceTo, ""
	cmd := cli.NewRootCommand(cli.Config{Fs: fsys, Store: &w.stores.Options})
	var stdout, stderr, tried bytes.Buffer
	var cue *interrupter
	ctx, cancel := context.WithCancel(w.t.Context())
	defer cancel()
	if w.outputClosed && (w.interruptOn != "" || w.interruptIn != 0 || w.interruptBy != 0) {
		w.t.Fatal("a command whose standard output is closed stops on its first write, before any interrupt")
	}
	switch {
	case w.outputClosed:
		cmd.SetOut(closedOutput{&tried})
	case w.interruptOn != "":
		cue = &interrupter{out: &stdout, text: []byte(w.interruptOn), ctx: ctx, cancel: cancel}
		cmd.SetOut(cue)
	default:
		cmd.SetOut(&stdout)
	}
	interruptIn, interruptBy := w.interruptIn, w.interruptBy
	w.interruptOn, w.interruptIn, w.interruptBy = "", 0, 0 // for this command alone
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{}, args...)) // never nil, which would make Cobra read the test's own arguments
	w.ran = true
	if w.ownStores {
		w.cachedBefore = w.cached()
	}
	asked, jobs, served := w.asked.Load(), w.jobs.Load(), w.served.Load()
	for _, d := range []time.Duration{interruptIn, interruptBy} {
		if d > 0 { // as the command starts, after anything slow beforehand
			timer := time.AfterFunc(d, cancel)
			defer timer.Stop()
		}
	}
	start := time.Now()
	w.err = cmd.ExecuteContext(ctx)
	w.took = time.Since(start)
	w.askedDuring, w.jobsDuring = w.asked.Load()-asked, w.jobs.Load()-jobs
	w.servedDuring = w.served.Load() - served
	if w.ownStores {
		w.cachedAfter = w.cached()
	}
	w.stdout, w.stderr, w.tried = stdout.String(), stderr.String(), tried.String()
	w.printedCue = cue != nil && cue.fired
	w.outputClosed = false // for this command alone
}

// project returns a new filesystem holding the scenario's project: in memory, unless it holds links
// or a step keeps it on disk.
func (w *world) project() afero.Fs {
	w.t.Helper()
	var fsys afero.Fs
	if len(w.links) > 0 || w.onDisk || w.linked {
		links := links{}
		for path, target := range w.links {
			links[path] = w.atOutside(target)
		}
		fsys, w.root = testenv.DiskProject(w.t, w.files, links)
		if w.linked {
			project := filepath.Join(w.root, testenv.ProjectDir)
			if err := os.Rename(project, project+"-real"); err != nil {
				w.t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(project)+"-real", project); err != nil {
				w.t.Fatal(err)
			}
		}
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
	owner   string    // its owner and group, as "uid:gid"
}

// outsideFile is a file that GivenAFileOutsideTheProjectHolds wrote: what it held, and what Lstat said of it then.
type outsideFile struct {
	content string
	info    os.FileInfo
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
