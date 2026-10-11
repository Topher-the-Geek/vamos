package internal

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// These tests check matching and the walk on their own, since they have more cases than builds could cover
// quickly. The project is in memory, but for symbolic links, which afero.MemMapFs lacks.

// testSelection returns a selection of the files within projectDir that include matches and exclude doesn't,
// with the defaults, less what change, if not nil, changes.
func testSelection(t *testing.T, include, exclude []string, change func(sel *selection)) *selection {
	t.Helper()
	sel := defaultSelection()
	sel.Include, sel.Exclude = include, exclude
	if change != nil {
		change(sel)
	}
	must(t, sel.compile(projectDir))
	return sel
}

// failedWith reports whether wantErr is set, failing t unless err holds it then.
func failedWith(t *testing.T, err error, wantErr string) bool {
	t.Helper()
	if wantErr == "" {
		must(t, err)
		return false
	}
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("error = %v, want %q", err, wantErr)
	}
	return true
}

// checkTree fails t unless the walk that returned got and err failed with wantErr, if set, or else made the
// tree that describeTree describes as want.
func checkTree(t *testing.T, got *tree, err error, want []string, wantErr string) {
	t.Helper()
	if failedWith(t, err, wantErr) {
		return
	}
	if lines := describeTree(got); !slices.Equal(lines, want) {
		t.Errorf("tree:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// describeTree lists t's entries as lines such as "file src/a.ts 644 content", for comparing in tests.
// A file's blob, from walkFiles, holds its content as its digest's hex.
func describeTree(t *tree) []string {
	var lines []string
	for _, e := range t.Entries {
		switch e.Type {
		case entryFile:
			lines = append(lines, fmt.Sprintf("file %s %o %s", e.Path, e.Mode, e.Blob.Digest.Encoded()))
		case entryDir:
			lines = append(lines, fmt.Sprintf("dir %s %o", e.Path, e.Mode))
		case entryLink:
			lines = append(lines, fmt.Sprintf("link %s -> %s", e.Path, e.Target))
		}
	}
	return lines
}

// walkFiles walks sel within projectDir in fsys, giving each file a blob that holds its content in place of a
// digest, so that describeTree shows which file each entry came from.
func walkFiles(fsys afero.Fs, sel *selection) (*tree, error) {
	return walkTree(fsys, projectDir, sel, func(name string, _ int64) (blob, error) {
		data, err := afero.ReadFile(fsys, name)
		return blob{Digest: digest.NewDigestFromEncoded(digest.SHA256, string(data)), Size: int64(len(data))}, err
	})
}

func TestMatchParts(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		name    string
		want    bool
	}{
		{pattern: "*.ts", name: "a.ts", want: true},
		{pattern: "*.ts", name: "src/a.ts", want: false},
		{pattern: "*", name: ".env", want: true},
		{pattern: "src/*", name: "src/sub/a.ts", want: false},
		{pattern: "src/**", name: "src", want: true},
		{pattern: "src/**", name: "src/sub/a.ts", want: true},
		{pattern: "src/**", name: "srcs/a.ts", want: false},
		{pattern: "**/*.ts", name: "a.ts", want: true},
		{pattern: "**/*.ts", name: "src/sub/a.ts", want: true},
		{pattern: "**/*.ts", name: "src/a.js", want: false},
		{pattern: "src/**/test/*.ts", name: "src/test/a.ts", want: true},
		{pattern: "src/**/test/*.ts", name: "src/x/y/test/a.ts", want: true},
		{pattern: "src/**/test/*.ts", name: "src/x/test/y/a.ts", want: false},
		{pattern: "**/node_modules", name: "ui/node_modules", want: true},
		{pattern: "file?.[ab]", name: "file1.a", want: true},
		{pattern: "file?.[ab]", name: "file12.a", want: false},
		{pattern: "file?.[ab]", name: "file1.c", want: false},
		{pattern: "src/a.ts", name: "src/a.ts", want: true},
		{pattern: "src", name: "src/a.ts", want: true}, // a path without wildcards includes what's beneath it
		{pattern: ".", name: "src/a.ts", want: true},
	} {
		t.Run(tc.pattern+" "+tc.name, func(t *testing.T) {
			p, err := parsePattern("include", tc.pattern, projectDir)
			must(t, err)
			if got := matchParts(p.parts, pathParts(tc.name)); got != tc.want {
				t.Errorf("matchParts(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
			}
		})
	}
}

func TestMayMatchBeneath(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		dir     string
		want    bool
	}{
		{pattern: "*.ts", dir: "src", want: false},
		{pattern: "src/*.ts", dir: "src", want: true},
		{pattern: "src/*.ts", dir: "lib", want: false},
		{pattern: "src/*.ts", dir: "src/sub", want: false},
		{pattern: "src/**", dir: "src/sub/deeper", want: true},
		{pattern: "**/*.ts", dir: "node_modules", want: true},
		{pattern: "*/test/*.ts", dir: "pkg", want: true},
		{pattern: "*/test/*.ts", dir: "pkg/test", want: true},
		{pattern: "*/test/*.ts", dir: "pkg/lib", want: false},
		{pattern: "src/a.ts", dir: "src", want: true},
		{pattern: "src/a.ts", dir: "lib", want: false},
	} {
		t.Run(tc.pattern+" "+tc.dir, func(t *testing.T) {
			p, err := parsePattern("include", tc.pattern, projectDir)
			must(t, err)
			if got := mayMatchBeneath(p.parts, pathParts(tc.dir)); got != tc.want {
				t.Errorf("mayMatchBeneath(%q, %q) = %v, want %v", tc.pattern, tc.dir, got, tc.want)
			}
		})
	}
}

func TestParsePattern(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		prefix  string
		err     string
	}{
		{pattern: "*.ts", prefix: "."},
		{pattern: "src/*.ts", prefix: "src"},
		{pattern: "src/a/**/b/*.ts", prefix: "src/a"},
		{pattern: "./src//a.ts", prefix: "src/a.ts"},
		{pattern: ".", prefix: "."},
		{pattern: `src/\*`, prefix: "src"},
		{pattern: "src/a**", err: `include "src/a**": "**" must be a whole part of the path`},
		{pattern: "src/[a", err: `include "src/[a": syntax error in pattern`},
		{pattern: "/src", err: `include "/src" must be a path relative to /project`},
		{pattern: "src/../../x", err: `include "src/../../x" is outside /project`},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			p, err := parsePattern("include", tc.pattern, projectDir)
			if failedWith(t, err, tc.err) {
				return
			}
			if p.prefix != tc.prefix {
				t.Errorf("prefix = %q, want %q", p.prefix, tc.prefix)
			}
		})
	}
}

func TestWalkTree(t *testing.T) {
	files := map[string]string{
		"top.ts":                     "top",
		"x.js":                       "x",
		".env":                       "env",
		"src/a.ts":                   "a",
		"src/b.js":                   "b",
		"src/sub/c.ts":               "c",
		"src/sub/empty.ts":           "",
		"node_modules/dep/index.js":  "dep",
		"ui/node_modules/dep/ui.js":  "ui-dep",
		"ui/main.ts":                 "ui",
		"file1.a":                    "1a",
		"file2.c":                    "2c",
		"a/x.txt":                    "x",
		"modes/open.txt":             "open",
		"modes/setuid":               "setuid",
		"modes/wide/inner.txt":       "inner",
		"limits/one.txt":             "1",
		"limits/three.txt":           "333",
		"limits/two/two.txt":         "22",
		"limits/two/also-two.txt.no": "--",
	}
	modes := map[string]fs.FileMode{
		"modes/open.txt": 0o666,
		"modes/setuid":   0o755 | fs.ModeSetuid,
		"modes/wide":     0o777 | fs.ModeDir,
	}
	// What "limits/**/*.txt" selects, within each limit tested.
	limitsTree := []string{
		"file limits/one.txt 644 1",
		"file limits/three.txt 644 333",
		"dir limits/two 755",
		"file limits/two/two.txt 644 22",
	}
	for _, tc := range []struct {
		name    string
		include []string
		exclude []string
		change  func(sel *selection)
		want    []string
		err     string
	}{
		{
			name:    "pattern in a directory",
			include: []string{"src/*.ts"},
			want:    []string{"file src/a.ts 644 a"},
		},
		{
			name:    "double star, with the directories beneath the prefix",
			include: []string{"src/**/*.ts"},
			want: []string{
				"file src/a.ts 644 a",
				"dir src/sub 755",
				"file src/sub/c.ts 644 c",
				"file src/sub/empty.ts 644 ",
			},
		},
		{
			name:    "top level only",
			include: []string{"*.ts"},
			want:    []string{"file top.ts 644 top"},
		},
		{
			name:    "path to a directory",
			include: []string{"src"},
			want: []string{
				"file src/a.ts 644 a",
				"file src/b.js 644 b",
				"dir src/sub 755",
				"file src/sub/c.ts 644 c",
				"file src/sub/empty.ts 644 ",
			},
		},
		{
			name:    "path to a file",
			include: []string{"src/sub/c.ts"},
			want:    []string{"file src/sub/c.ts 644 c"},
		},
		{
			name:    "dotfiles",
			include: []string{"*"},
			exclude: []string{"*.?s"},
			want:    []string{"file .env 644 env", "file file1.a 644 1a", "file file2.c 644 2c"},
		},
		{
			name:    "character class",
			include: []string{"file?.[ab]"},
			want:    []string{"file file1.a 644 1a"},
		},
		{
			name:    "exclude is relative to the directory",
			include: []string{"src/**", "*.js"},
			exclude: []string{"*.js"},
			want: []string{
				"file src/a.ts 644 a",
				"file src/b.js 644 b",
				"dir src/sub 755",
				"file src/sub/c.ts 644 c",
				"file src/sub/empty.ts 644 ",
			},
		},
		{
			name:    "exclude anywhere",
			include: []string{"src/**"},
			exclude: []string{"**/*.js", "**/empty.ts"},
			want:    []string{"file src/a.ts 644 a", "dir src/sub 755", "file src/sub/c.ts 644 c"},
		},
		{
			name:    "exclude a directory",
			include: []string{"**/*.js", "**/*.ts"},
			exclude: []string{"**/node_modules", "src", "top.ts"},
			want:    []string{"dir ui 755", "file ui/main.ts 644 ui", "file x.js 644 x"},
		},
		{
			// The walk starts at "ui/node_modules/dep", beneath the excluded directory, but must skip it all
			// the same, as it does when it starts above it.
			name:    "exclude a directory above a pattern's prefix",
			include: []string{"ui/node_modules/dep/*.js", "src/a.ts"},
			exclude: []string{"**/node_modules"},
			want:    []string{"file src/a.ts 644 a"},
		},
		{
			name:    "exclude a directory that is a pattern's prefix",
			include: []string{"ui/node_modules/dep/*.js", "src/a.ts"},
			exclude: []string{"ui/node_*/dep"},
			want:    []string{"file src/a.ts 644 a"},
		},
		{
			name:    "overlapping patterns, each file once",
			include: []string{"src/**", "**/c.ts", "src/a.ts"},
			want: []string{
				"dir src 755",
				"file src/a.ts 644 a",
				"file src/b.js 644 b",
				"dir src/sub 755",
				"file src/sub/c.ts 644 c",
				"file src/sub/empty.ts 644 ",
			},
		},
		{
			// src/sub/c.ts, matched first, records only what's beneath src; src/sub/empty.ts records src too.
			name:    "a file of a pattern with an outer prefix, after one of a deeper prefix",
			include: []string{"src/*/c.ts", "**/empty.ts"},
			want: []string{
				"dir src 755",
				"dir src/sub 755",
				"file src/sub/c.ts 644 c",
				"file src/sub/empty.ts 644 ",
			},
		},
		{
			// "." and "a" are as long as each other, but "." holds a.
			name:    "the outermost prefix, whatever the order of patterns",
			include: []string{"a/**", "**/x.txt"},
			want:    []string{"dir a 755", "file a/x.txt 644 x"},
		},
		{
			// modes is copied with all it holds, so modes/wide must be in the tree, with its own mode, though it is
			// the prefix of the pattern that selects inner.txt.
			name:    "a directory between a recorded one and a deeper prefix's files",
			include: []string{"**/open.txt", "modes/wide/inner.txt"},
			change:  func(sel *selection) { sel.ModeMask = 0o7777 },
			want: []string{
				"dir modes 755",
				"file modes/open.txt 666 open",
				"dir modes/wide 777",
				"file modes/wide/inner.txt 644 inner",
			},
		},
		{
			name:    "mode mask",
			include: []string{"modes"},
			want: []string{
				"file modes/open.txt 644 open",
				"file modes/setuid 755 setuid",
				"dir modes/wide 755",
				"file modes/wide/inner.txt 644 inner",
			},
		},
		{
			name:    "mode mask keeping every bit",
			include: []string{"modes"},
			change:  func(sel *selection) { sel.ModeMask = 0o7777 },
			want: []string{
				"file modes/open.txt 666 open",
				"file modes/setuid 4755 setuid",
				"dir modes/wide 777",
				"file modes/wide/inner.txt 644 inner",
			},
		},
		{
			name:    "no files",
			include: []string{"*.go", "src/*.go"},
			err:     `no files in /project match include "*.go", "src/*.go"; pass allow_no_files=True`,
		},
		{
			name:    "no files, allowed",
			include: []string{"*.go"},
			change:  func(sel *selection) { sel.AllowNoFiles = true },
		},
		{
			name:    "everything excluded",
			include: []string{"src"},
			exclude: []string{"**/*.ts", "**/*.js"},
			err:     `no files in /project match include "src" but not exclude "**/*.js", "**/*.ts"; pass`,
		},
		{
			name:    "missing prefix",
			include: []string{"*.ts", "scr/**"},
			err:     `include "scr/**": "scr" not found in /project`,
		},
		{
			name:    "missing file",
			include: []string{"src/d.ts"},
			err:     `include "src/d.ts" not found in /project`,
		},
		{
			name:    "prefix is a file",
			include: []string{"top.ts/**"},
			want:    []string{"file top.ts 644 top"},
		},
		{
			name:    "file count at the limit",
			include: []string{"limits/**/*.txt"},
			change:  func(sel *selection) { sel.MaxFileCount = 3 },
			want:    limitsTree,
		},
		{
			name:    "too many files",
			include: []string{"limits/**/*.txt"},
			change:  func(sel *selection) { sel.MaxFileCount = 2 },
			err:     "more than max_file_count=2 files in /project match; exclude some, or raise the limit",
		},
		{
			name:    "file size at the limit",
			include: []string{"limits/**/*.txt"},
			change:  func(sel *selection) { sel.MaxFileSize = 3 },
			want:    limitsTree,
		},
		{
			name:    "file too big",
			include: []string{"limits/**/*.txt"},
			change:  func(sel *selection) { sel.MaxFileSize = 2 },
			err:     "/project/limits/three.txt is 3 bytes, more than max_file_size=2",
		},
		{
			name:    "total size at the limit",
			include: []string{"limits/**/*.txt"},
			change:  func(sel *selection) { sel.MaxTotalSize = 6 },
			want:    limitsTree,
		},
		{
			name:    "total too big",
			include: []string{"limits/**/*.txt"},
			change:  func(sel *selection) { sel.MaxTotalSize = 5 },
			err:     "the files in /project that match total more than max_total_size=5 bytes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := project(t, files)
			for name, mode := range modes {
				must(t, fsys.Chmod(filepath.Join(projectDir, name), mode))
			}
			got, err := walkFiles(fsys, testSelection(t, tc.include, tc.exclude, tc.change))
			checkTree(t, got, err, tc.want, tc.err)
		})
	}
}

// TestWalkTreeFileGrew checks that the limits apply to a file as it is read, not as it was described before,
// so that a file that grows meanwhile can't bring more into the cache than they allow.
func TestWalkTreeFileGrew(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     int // the time a.txt is opened at which it grows: 1 as it is hashed, 2 as it is stored
		change func(sel *selection)
		err    string
	}{
		{
			name:   "past max_file_size as hashed",
			at:     1,
			change: func(sel *selection) { sel.MaxFileSize = 3 },
			err:    "/project/a.txt grew to more than max_file_size=3 bytes as it was read",
		},
		{
			name:   "past max_file_size as stored",
			at:     2,
			change: func(sel *selection) { sel.MaxFileSize = 3 },
			err:    "/project/a.txt grew to more than max_file_size=3 bytes as it was read",
		},
		{
			name:   "past max_total_size",
			at:     1,
			change: func(sel *selection) { sel.MaxTotalSize = 3 },
			err:    "the files in /project that match total more than max_total_size=3 bytes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := &editedFs{
				Fs:   project(t, map[string]string{"a.txt": "a", "b.txt": "b"}),
				name: "/project/a.txt",
				edit: "grown",
				at:   tc.at,
			}
			c := newTestCache(t)
			_, err := c.putFiles(fsys, projectDir, testSelection(t, []string{"*.txt"}, nil, tc.change))
			failedWith(t, err, tc.err)
			if n := countBlobs(t, c); n != 0 {
				t.Errorf("the cache holds %d blobs, want none", n)
			}
		})
	}
}

// TestSelectionKey checks that a selection's patterns are kept the same however they were written and in
// whatever order, since neither changes the files they select, so a task's key doesn't change either.
func TestSelectionKey(t *testing.T) {
	a := testSelection(t, []string{"src/**", "./b/", "src/**"}, []string{"**/x", "a/../y"}, nil)
	b := testSelection(t, []string{"b", "src/**"}, []string{"y", "**/x"}, nil)
	if !slices.Equal(a.Include, b.Include) || !slices.Equal(a.Exclude, b.Exclude) {
		t.Errorf("patterns %q, %q and %q, %q, want them the same", a.Include, a.Exclude, b.Include, b.Exclude)
	}
}

// touchFs records each path that is opened or described.
type touchFs struct {
	afero.Fs
	touched []string
}

// Open records name, then opens it.
func (f *touchFs) Open(name string) (afero.File, error) {
	f.touched = append(f.touched, name)
	return f.Fs.Open(name)
}

// Stat records name, then describes it.
func (f *touchFs) Stat(name string) (fs.FileInfo, error) {
	f.touched = append(f.touched, name)
	return f.Fs.Stat(name)
}

// LstatIfPossible records name, then describes it without following a final link.
func (f *touchFs) LstatIfPossible(name string) (fs.FileInfo, bool, error) {
	f.touched = append(f.touched, name)
	return lstat(f.Fs, name)
}

// TestWalkTreeReadsOnlyWhatItNeeds checks that the walk reads only the directories that its include patterns
// could match within and its exclude patterns don't match, so a project's other folders, such as a large
// node_modules, cost nothing.
func TestWalkTreeReadsOnlyWhatItNeeds(t *testing.T) {
	files := map[string]string{
		"src/a.ts":                  "a",
		"src/sub/b.ts":              "b",
		"node_modules/dep/index.js": "dep",
		"ui/node_modules/dep/ui.js": "ui-dep",
		"ui/main.ts":                "ui",
		"docs/README.md":            "docs",
	}
	for _, tc := range []struct {
		name    string
		include []string
		exclude []string
		touched []string // relative to the project
	}{
		{
			name:    "from the prefix",
			include: []string{"src/**"},
			touched: []string{"src", "src/a.ts", "src/sub", "src/sub/b.ts"},
		},
		{
			// Each directory read is listed, so its entries are described, but only those that can match are read.
			name:    "only directories that can match",
			include: []string{"ui/*.ts", "src/*.ts"},
			touched: []string{"src", "src/a.ts", "src/sub", "ui", "ui/main.ts", "ui/node_modules"},
		},
		{
			name:    "not excluded directories",
			include: []string{"**/*.ts"},
			exclude: []string{"**/node_modules"},
			touched: []string{
				".",
				"docs",
				"docs/README.md",
				"node_modules",
				"src",
				"src/a.ts",
				"src/sub",
				"src/sub/b.ts",
				"ui",
				"ui/main.ts",
				"ui/node_modules",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := &touchFs{Fs: project(t, files)}
			_, err := walkFiles(fsys, testSelection(t, tc.include, tc.exclude, nil))
			must(t, err)
			var touched []string
			for _, name := range fsys.touched {
				rel, err := filepath.Rel(projectDir, name)
				must(t, err)
				touched = append(touched, rel)
			}
			slices.Sort(touched)
			if touched = slices.Compact(touched); !slices.Equal(touched, tc.touched) {
				t.Errorf("walk touched %q, want %q", touched, tc.touched)
			}
		})
	}
}

// TestWalkTreeLinks checks that the walk records links as links, never following them, and refuses a link on
// the way to a pattern's prefix.
func TestWalkTreeLinks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		include []string
		want    []string
		err     string
	}{
		{
			name:    "links within",
			include: []string{"site/**"},
			want: []string{
				"link site/home.html -> index.html",
				"file site/index.html 644 hi",
				"link site/lib -> ../shared",
				"link site/secret -> /etc/hostname",
			},
		},
		{
			name:    "a link that matches",
			include: []string{"site/l*"},
			want:    []string{"link site/lib -> ../shared"},
		},
		{
			name:    "never followed",
			include: []string{"site/lib/**"},
			err:     `include "site/lib/**": "site/lib" is a symbolic link; include what it links to instead`,
		},
		{
			name:    "link on the way to the prefix",
			include: []string{"linked/x/*.txt"},
			err:     `include "linked/x/*.txt": "linked/x" has symbolic link parent "/project/linked"`,
		},
		{
			name:    "path to a link",
			include: []string{"site/secret"},
			err:     `include "site/secret" is a symbolic link`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, _ := diskProject(t, map[string]string{
				"site/index.html":  "hi",
				"shared/lib.js":    "lib",
				"shared/x/y.txt":   "y",
				"other/escape.txt": "escape",
			}, map[string]string{
				"site/home.html": "index.html",
				"site/secret":    "/etc/hostname",
				"site/lib":       "../shared",
				"linked":         "shared",
			})
			got, err := walkFiles(fsys, testSelection(t, tc.include, nil, nil))
			checkTree(t, got, err, tc.want, tc.err)
		})
	}
}

// TestWalkTreeThroughLinkedDir checks that the directory walked may itself be reached through a link, as a
// project is from a linked $PWD.
func TestWalkTreeThroughLinkedDir(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "real"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "real", "a.txt"), []byte("a"), 0o644))
	must(t, os.Symlink("real", filepath.Join(root, "project")))
	fsys := afero.NewBasePathFs(afero.NewOsFs(), root)

	got, err := walkFiles(fsys, testSelection(t, []string{"*.txt"}, nil, nil))
	checkTree(t, got, err, []string{"file a.txt 644 a"}, "")
}
