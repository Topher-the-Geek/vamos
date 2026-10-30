package features

import (
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
)

// Steps about the folder that a command sends to, and the files that export writes there.

// GivenTheDestinationHolds puts files, by path, in the folder that the next command sends to, and none for none.
func (w *world) GivenTheDestinationHolds(destination files) {
	w.t.Helper()
	for path, content := range destination {
		path = filepath.Join(w.destination(), path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
}

// GivenTheDestinationModes gives the files and folders at these paths, in the folder that the next command sends to,
// these modes.
func (w *world) GivenTheDestinationModes(destination modes) {
	w.t.Helper()
	for path, mode := range destination {
		if err := os.Chmod(filepath.Join(w.destination(), path), fs.FileMode(mode)); err != nil {
			w.t.Fatal(err)
		}
	}
}

// GivenTheDestinationHasTheFolders makes an empty folder at each of paths in the folder that the next command
// sends to, and none for none.
func (w *world) GivenTheDestinationHasTheFolders(paths ...string) {
	w.t.Helper()
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(w.destination(), path), 0o755); err != nil {
			w.t.Fatal(err)
		}
	}
}

// GivenTheNextCommandSendsWhereTheLastDid makes the next command send to the folder that the last command sent to,
// as it is, so that a scenario can follow a folder from one command to the next.
func (w *world) GivenTheNextCommandSendsWhereTheLastDid() {
	w.t.Helper()
	if w.dest == "" {
		w.t.Fatal("no command has run that sends images or files")
	}
	w.prepared = w.dest
}

// destination returns the folder that the next command sends to, making it if no Given step has.
func (w *world) destination() string {
	if w.prepared == "" {
		w.prepared = w.t.TempDir()
	}
	return w.prepared
}

// pathsOf returns the paths of holds and folders, relative to a folder, and of the folders that lead to them,
// for a scenario to expect of a destination as Given steps filled it.
func pathsOf(holds files, folders []string) []string {
	paths := map[string]bool{}
	for _, p := range append(slices.Collect(maps.Keys(holds)), folders...) {
		for p = path.Clean(p); p != "." && p != "/"; p = path.Dir(p) {
			paths[p] = true
		}
	}
	return slices.Collect(maps.Keys(paths))
}

// ThenNothingIsSaved checks that the folder that the last command sent to holds nothing: no file, folder or link.
// It stops the scenario if no command has run that sends images or files, or if the last saved into the project.
func (w *world) ThenNothingIsSaved() {
	w.t.Helper()
	w.nothingSent()
}

// ThenNothingIsExported checks that the folder that the last command sent to holds nothing: no file, folder or link.
// It stops the scenario if no command has run that sends images or files, or if the last saved into the project.
func (w *world) ThenNothingIsExported() {
	w.t.Helper()
	w.nothingSent()
}

// nothingSent checks that the last command left nothing where it sends images and files.
func (w *world) nothingSent() {
	w.t.Helper()
	w.check(matchPaths(withoutRoot(w.destinationEntries()), nil))
}

// ThenTheExportHolds checks that the regular files in the folder exported to are exactly want, by path,
// and that every folder in it holds one of them: a symbolic link, another file or an empty folder fails it.
// An export that holds links is listed with ThenTheExportPathsAre instead, its links checked by ThenTheExportLinks
// and its files by ThenTheExportHoldsAtLeast.
// Like every step about the export, it stops the scenario if the last command exported nothing, or it can't be read.
func (w *world) ThenTheExportHolds(want files) {
	w.t.Helper()
	w.check(matchFiles(withoutRoot(w.exportedEntries()), want, true))
}

// ThenTheExportHoldsAtLeast checks that the folder exported to holds want, by path, and allows other files.
func (w *world) ThenTheExportHoldsAtLeast(want files) {
	w.t.Helper()
	w.check(matchFiles(w.exportedEntries(), want, false))
}

// ThenTheExportPathsAre checks that the paths in the folder exported to, folders and links among them,
// are exactly want.
func (w *world) ThenTheExportPathsAre(want ...string) {
	w.t.Helper()
	w.check(matchPaths(withoutRoot(w.exportedEntries()), want))
}

// ThenTheExportLinks checks that the symbolic links in the folder exported to are exactly want, by path,
// to their targets.
func (w *world) ThenTheExportLinks(want links) {
	w.t.Helper()
	w.check(matchLinks(w.exportedEntries(), want, true))
}

// ThenTheExportModesAre checks the modes of the files and folders exported at want's paths, "." for the folder
// exported to, and checks no others.
func (w *world) ThenTheExportModesAre(want modes) {
	w.t.Helper()
	w.check(matchModes(w.exportedEntries(), want))
}

// ThenTheExportFilesWereModifiedAt checks the modification times of what was exported at paths,
// "." for the folder exported to, and checks no others.
func (w *world) ThenTheExportFilesWereModifiedAt(want time.Time, paths ...string) {
	w.t.Helper()
	w.check(matchModTimes(w.exportedEntries(), want, paths))
}

// ThenTheDestinationHolds checks that the regular files in the folder that the command sent to are exactly want,
// by path, and that every folder in it holds one of them: a symbolic link, another file or an empty folder fails it.
// Like every step about the destination, it stops the scenario if no command has run that sends images or files,
// or if the last saved into the project.
func (w *world) ThenTheDestinationHolds(want files) {
	w.t.Helper()
	w.check(matchFiles(withoutRoot(w.destinationEntries()), want, true))
}

// ThenTheDestinationHoldsAtLeast checks that the folder that the command sent to holds want, by path,
// and allows other files.
func (w *world) ThenTheDestinationHoldsAtLeast(want files) {
	w.t.Helper()
	w.check(matchFiles(w.destinationEntries(), want, false))
}

// ThenTheDestinationModesAre checks the modes of the files and folders at want's paths,
// in the folder that the command sent to, and checks no others.
func (w *world) ThenTheDestinationModesAre(want modes) {
	w.t.Helper()
	w.check(matchModes(w.destinationEntries(), want))
}

// ThenTheDestinationPathsAre checks that the paths in the folder that the command sent to,
// folders among them, are exactly want.
func (w *world) ThenTheDestinationPathsAre(want ...string) {
	w.t.Helper()
	w.check(matchPaths(withoutRoot(w.destinationEntries()), want))
}

func (w *world) destinationEntries() map[string]entry {
	w.t.Helper()
	if w.dest == "" {
		w.t.Fatal("no command has run that sends images or files")
	}
	if w.inProject {
		w.t.Fatal("the last command saved into the project, whose other files it didn't send")
	}
	return w.folderEntries(w.dest)
}

// exportedEntries returns the entries of the folder exported to, by path relative to it.
func (w *world) exportedEntries() map[string]entry {
	w.t.Helper()
	if w.exported == "" {
		w.t.Fatal("nothing was exported")
	}
	return w.folderEntries(w.exported)
}

// folderEntries returns the entries in dir, by path relative to it, with "." for dir itself.
func (w *world) folderEntries(dir string) map[string]entry {
	w.t.Helper()
	entries := map[string]entry{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		e := entry{
			dir:     d.IsDir(),
			regular: d.Type().IsRegular(),
			symlink: d.Type()&fs.ModeSymlink != 0,
			mode:    int64(cache.ChmodBits(info)),
			modTime: info.ModTime(),
		}
		switch {
		case e.regular:
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			e.content = string(b)
		case e.symlink:
			if e.target, err = os.Readlink(path); err != nil {
				return err
			}
		}
		entries[filepath.ToSlash(rel)] = e
		return nil
	})
	if err != nil {
		w.t.Fatalf("reading %s: %v", dir, err)
	}
	return entries
}

// withoutRoot returns a copy of entries without ".", the folder they are in.
func withoutRoot(entries map[string]entry) map[string]entry {
	entries = maps.Clone(entries)
	delete(entries, ".")
	return entries
}
