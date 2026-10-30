package features

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// TestDedent checks that a BUILD file indented with the Go that declares it loses only that indentation.
func TestDedent(t *testing.T) {
	got := dedent(`
		task(
		    name="make",
		    steps=[run(["sh", "-c", """
		        echo hi
		    """])],

		)
	`)
	want := "task(\n    name=\"make\",\n    steps=[run([\"sh\", \"-c\", \"\"\"\n        echo hi\n    \"\"\"])],\n\n)\n"
	if got != want {
		t.Errorf("dedent = %q, want %q", got, want)
	}
	if got := dedent(`layer(name="app")`); got != "layer(name=\"app\")\n" {
		t.Errorf("dedent of one line = %q", got)
	}
	if got := dedent("  \n\t\tlayer(name=\"app\")\n\t \n"); got != "layer(name=\"app\")\n" {
		t.Errorf("dedent between lines of spaces and tabs = %q", got)
	}
}

// TestMatchFiles checks the comparisons that the image and export steps make, since every scenario relies on them:
// exactly and at least, of files and of links.
func TestMatchFiles(t *testing.T) {
	entries := map[string]entry{
		"a.txt":     {regular: true, mode: 0o644, content: "a\n"},
		"sub":       {dir: true, mode: 0o755},
		"sub/b.txt": {regular: true, mode: 0o755, content: "b\n"},
	}
	for _, tc := range []struct {
		name    string
		extra   map[string]entry
		want    files
		links   links
		modes   modes
		exactly bool // whether want lists every file
		// whether links lists every link
		linksExactly bool
		errs         []string
	}{
		{
			name: "every file listed",
			want: files{
				"a.txt":     "a\n",
				"sub/b.txt": "b\n",
			},
			modes: modes{
				"a.txt": 0o644,
				"sub":   0o755,
			},
			exactly: true,
		},
		{
			name:  "an unlisted file",
			extra: map[string]entry{"c.txt": {regular: true}},
			want: files{
				"a.txt":     "a\n",
				"sub/b.txt": "b\n",
			},
			exactly: true,
			errs:    []string{"c.txt is there too"},
		},
		{
			name:  "an unlisted file is allowed without exactly",
			extra: map[string]entry{"c.txt": {regular: true}},
			want:  files{"a.txt": "a\n"},
		},
		{
			name:  "an empty directory",
			extra: map[string]entry{"empty": {dir: true}},
			want: files{
				"a.txt":     "a\n",
				"sub/b.txt": "b\n",
			},
			exactly: true,
			errs:    []string{"empty is there too"},
		},
		{
			name:  "a directory whose name prefixes a listed file's",
			extra: map[string]entry{"su": {dir: true}},
			want: files{
				"a.txt":     "a\n",
				"sub/b.txt": "b\n",
			},
			exactly: true,
			errs:    []string{"su is there too"},
		},
		{
			name:  "a link",
			extra: map[string]entry{"link": {symlink: true, target: "a.txt"}},
			want:  files{"link": ""},
			errs:  []string{"link is not a regular file"},
		},
		{
			name:  "a link not listed",
			extra: map[string]entry{"link": {symlink: true, target: "a.txt"}},
			want: files{
				"a.txt":     "a\n",
				"sub/b.txt": "b\n",
			},
			exactly: true,
			errs:    []string{"link is there too"},
		},
		{
			name: "every link listed, one in a directory of its own",
			extra: map[string]entry{
				"link":        {symlink: true, target: "a.txt"},
				"only":        {dir: true, mode: 0o755},
				"only/passwd": {symlink: true, target: "/etc/passwd"},
			},
			links: links{
				"link":        "a.txt",
				"only/passwd": "/etc/passwd",
			},
			linksExactly: true,
		},
		{
			name: "an unlisted link",
			extra: map[string]entry{
				"link":  {symlink: true, target: "a.txt"},
				"other": {symlink: true, target: "b"},
			},
			links:        links{"link": "a.txt"},
			linksExactly: true,
			errs:         []string{"other is there too"},
		},
		{
			name:  "an unlisted link is allowed without exactly",
			extra: map[string]entry{"link": {symlink: true, target: "a.txt"}, "other": {symlink: true, target: "b"}},
			links: links{"link": "a.txt"},
		},
		{
			name:  "a directory holding only a link is there too, listing files",
			extra: map[string]entry{"only": {dir: true}, "only/passwd": {symlink: true, target: "/etc/passwd"}},
			want: files{
				"a.txt":     "a\n",
				"sub/b.txt": "b\n",
			},
			exactly: true,
			errs:    []string{"only is there too", "only/passwd is there too"},
		},
		{
			name:  "a link to elsewhere, and a file listed as a link",
			extra: map[string]entry{"link": {symlink: true, target: "b.txt"}},
			links: links{
				"link":  "a.txt",
				"a.txt": "b.txt",
			},
			errs: []string{`link links to "b.txt", want "a.txt"`, "a.txt is there, but not as a symbolic link"},
		},
		{
			name: "wrong content and modes and missing files, together",
			want: files{
				"a.txt":    "A\n",
				"gone.txt": "",
				"sub":      "",
			},
			modes: modes{
				"a.txt":     0o600,
				"sub":       0o700,
				"gone.txt":  0o644,
				"sub/b.txt": 0o755,
			},
			errs: []string{
				`a.txt = "a\n", want "A\n"`,
				"gone.txt not found",
				"sub is a directory",
				"mode of a.txt = 0644, want 0600",
				"mode of sub = 0755, want 0700",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := map[string]entry{}
			for path, e := range entries {
				all[path] = e
			}
			for path, e := range tc.extra {
				all[path] = e
			}
			err := errors.Join(
				matchFiles(all, tc.want, tc.exactly),
				matchLinks(all, tc.links, tc.linksExactly),
				matchModes(all, tc.modes),
			)
			if len(tc.errs) == 0 {
				if err != nil {
					t.Fatalf("matching = %v, want no error", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("matching succeeded, want %q", tc.errs)
			}
			for _, want := range tc.errs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("matching = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestReadImage checks that reading an image stacks its layers: a hard link takes its file as it is
// when the link's layer applies, in that layer or one below, through another link or not, whatever replaces it later;
// whiteouts hide what layers below hold, but not what their own layer holds, before or after them;
// a hard link whose target isn't a file before it is an error; and the image's root is left out.
func TestReadImage(t *testing.T) {
	img, err := mutate.AppendLayers(empty.Image,
		tarLayer(t,
			tarEntry{hdr: &tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}},
			tarEntry{hdr: &tar.Header{Name: "./bin/", Typeflag: tar.TypeDir, Mode: 0o755}},
			tarEntry{hdr: &tar.Header{Name: "./bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755}, content: "box\n"},
			tarEntry{hdr: &tar.Header{Name: "./bin/sh", Typeflag: tar.TypeLink, Linkname: "bin/busybox", Mode: 0o755}},
			tarEntry{hdr: &tar.Header{Name: "./etc/", Typeflag: tar.TypeDir, Mode: 0o755}},
			tarEntry{hdr: &tar.Header{Name: "./etc/old.conf", Typeflag: tar.TypeReg, Mode: 0o644}, content: "old\n"},
			tarEntry{hdr: &tar.Header{Name: "./gone", Typeflag: tar.TypeReg, Mode: 0o644}, content: "gone\n"},
			tarEntry{hdr: &tar.Header{Name: "./redone", Typeflag: tar.TypeReg, Mode: 0o644}, content: "old\n"},
			tarEntry{hdr: &tar.Header{Name: "./dir/", Typeflag: tar.TypeDir, Mode: 0o755}},
			tarEntry{hdr: &tar.Header{Name: "./dir/old", Typeflag: tar.TypeReg, Mode: 0o644}, content: "old\n"},
		),
		tarLayer(t,
			tarEntry{hdr: &tar.Header{Name: "./bin/mid", Typeflag: tar.TypeLink, Linkname: "bin/busybox", Mode: 0o755}},
			tarEntry{hdr: &tar.Header{Name: "./bin/ash", Typeflag: tar.TypeLink, Linkname: "bin/sh", Mode: 0o755}},
		),
		tarLayer(t,
			tarEntry{hdr: &tar.Header{Name: "./bin/busybox", Typeflag: tar.TypeReg, Mode: 0o700}, content: "new\n"},
			tarEntry{hdr: &tar.Header{Name: "./bin/new", Typeflag: tar.TypeLink, Linkname: "bin/busybox", Mode: 0o700}},
			tarEntry{hdr: &tar.Header{Name: "./.wh.gone", Typeflag: tar.TypeReg}},
			tarEntry{hdr: &tar.Header{Name: "./etc/new.conf", Typeflag: tar.TypeReg, Mode: 0o644}, content: "new\n"},
			tarEntry{hdr: &tar.Header{Name: "./etc/.wh..wh..opq", Typeflag: tar.TypeReg}},
			tarEntry{hdr: &tar.Header{Name: "./redone", Typeflag: tar.TypeReg, Mode: 0o644}, content: "new\n"},
			tarEntry{hdr: &tar.Header{Name: "./.wh.redone", Typeflag: tar.TypeReg}},
			tarEntry{hdr: &tar.Header{Name: "./dir/", Typeflag: tar.TypeDir, Mode: 0o755}},
			tarEntry{hdr: &tar.Header{Name: "./dir/new", Typeflag: tar.TypeReg, Mode: 0o644}, content: "new\n"},
			tarEntry{hdr: &tar.Header{Name: "./.wh.dir", Typeflag: tar.TypeReg}},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := readImage(img)
	if err != nil {
		t.Fatal(err)
	}
	err = errors.Join(
		matchFiles(entries, files{
			"/bin/busybox":  "new\n",
			"/bin/sh":       "box\n",
			"/bin/mid":      "box\n",
			"/bin/ash":      "box\n",
			"/bin/new":      "new\n",
			"/etc/new.conf": "new\n",
			"/redone":       "new\n",
			"/dir/new":      "new\n",
		}, true),
		matchModes(entries, modes{
			"/bin/sh":  0o755,
			"/bin/new": 0o700,
		}),
	)
	if err != nil {
		t.Error(err)
	}

	broken, err := mutate.AppendLayers(empty.Image, tarLayer(t,
		tarEntry{hdr: &tar.Header{Name: "./bin/sh", Typeflag: tar.TypeLink, Linkname: "bin/busybox", Mode: 0o755}},
	))
	if err != nil {
		t.Fatal(err)
	}
	want := "hard link /bin/sh: its target, bin/busybox, isn't a regular file before it"
	if _, err := readImage(broken); err == nil || err.Error() != want {
		t.Errorf("reading an image with a broken hard link = %v, want %q", err, want)
	}
}

// tarEntry is an entry of a layer's tar, with the content of a regular file.
type tarEntry struct {
	hdr     *tar.Header
	content string
}

// tarLayer returns a layer of entries, in order.
func tarLayer(t *testing.T, entries ...tarEntry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		e.hdr.Size = int64(len(e.content))
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

// TestUnconfigured checks that a label loses only the configuration after its quoted name,
// whatever the name holds.
func TestUnconfigured(t *testing.T) {
	for label, want := range map[string]string{
		layer("app"):                     `layer "app"`,
		layer("app", `mode="debug"`):     `layer "app"`,
		task("app (debug)"):              `task "app (debug)"`,
		task(`a "quoted" (name)`, "x=1"): `task "a \"quoted\" (name)"`,
	} {
		if got := unconfigured(label); got != want {
			t.Errorf("unconfigured(%s) = %s, want %s", label, got, want)
		}
	}
}

// TestMatchObservations checks that observations are read from whole lines of output, and matched exactly.
func TestMatchObservations(t *testing.T) {
	output := "building\nobserved key owner=0:0\nnot observed x=1\nobserved token=matches\r\nobserved empty=\n"
	if err := matchObservations(output, observations{"key owner": "0:0", "token": "matches", "empty": ""}); err != nil {
		t.Errorf("matching = %v, want no error", err)
	}
	mistakes := "observed token=differs\nobserved nothing\nobserved =x\nobserved key =x\n"
	err := matchObservations(output+mistakes, observations{
		"key owner": "1:1",
		"missing":   "x",
		"empty":     "",
	})
	wants := []string{
		`"key owner" observed "0:0", want "1:1"`,
		`"missing" not observed, want "x"`,
		`"token" observed "matches" too`,
		`"token" observed again, as "differs", after "matches"`,
		`"observed nothing" observes no key=value`,
		`"observed =x" observes no key=value`,
		`"observed key =x" observes no key=value`,
	}
	if err == nil || strings.Count(err.Error(), "\n")+1 != len(wants) {
		t.Errorf("matching = %v, want %d errors", err, len(wants))
	}
	for _, want := range wants {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("matching = %v, want it to contain %q", err, want)
		}
	}
}
