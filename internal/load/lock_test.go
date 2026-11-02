package load

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
)

// lockFilesystems are the filesystems that the lockfile's tests run on: in memory, and on disk, in a folder of the
// test's own, each with the root that the test's workspace has there.
func lockFilesystems(t *testing.T) map[string]func(t *testing.T) (afero.Fs, string) {
	return map[string]func(t *testing.T) (afero.Fs, string){
		"in memory": func(t *testing.T) (afero.Fs, string) {
			return testenv.Project(t, map[string]string{"WORKSPACE": ""}), testenv.ProjectDir
		},
		"on disk": func(t *testing.T) (afero.Fs, string) { return afero.NewOsFs(), t.TempDir() },
	}
}

// updateLock is UpdateLock, but for whether it wrote.
func updateLock(ctx context.Context, fsys afero.Fs, root string, change func(*Lock)) error {
	_, err := UpdateLock(ctx, fsys, root, change)
	return err
}

// pin returns a pin of tag to the digest of the string manifest.
func pin(tag, manifest string) Pin {
	return Pin{Tag: tag, Digest: digest.FromString(manifest)}
}

// TestLockWritingIsStable checks that the lockfile is written with sorted keys, one field per line, two spaces and a
// trailing newline, that writing the same pins again gives the same bytes and writes nothing, and that what is
// written reads back as it was.
func TestLockWritingIsStable(t *testing.T) {
	for name, open := range lockFilesystems(t) {
		t.Run(name, func(t *testing.T) {
			fsys, root := open(t)
			pins := map[string]Pin{
				"docker.io/library/node:24-alpine": pin("24.11.1-alpine", "node"),
				"ghcr.io/me/base:1.3":              pin("1.3.2", "base"),
			}
			testenv.Must(t, updateLock(t.Context(), fsys, root, func(l *Lock) {
				// Added in the reverse of their order, which the lockfile sorts.
				l.Pulls["ghcr.io/me/base:1.3"] = pins["ghcr.io/me/base:1.3"]
				l.Pulls["docker.io/library/node:24-alpine"] = pins["docker.io/library/node:24-alpine"]
			}))
			want := `{
  "version": 1,
  "pulls": {
    "docker.io/library/node:24-alpine": {
      "tag": "24.11.1-alpine",
      "digest": "` + digest.FromString("node").String() + `"
    },
    "ghcr.io/me/base:1.3": {
      "tag": "1.3.2",
      "digest": "` + digest.FromString("base").String() + `"
    }
  }
}
`
			got, err := afero.ReadFile(fsys, LockPath(root))
			testenv.Must(t, err)
			if string(got) != want {
				t.Fatalf("wrote\n%s\nwant\n%s", got, want)
			}
			info, err := fsys.Stat(LockPath(root))
			testenv.Must(t, err)
			if info.Mode().Perm() != 0o644 {
				t.Errorf("mode %v, want 0644", info.Mode().Perm())
			}

			testenv.Must(t, updateLock(t.Context(), fsys, root, func(l *Lock) {
				l.Pulls["ghcr.io/me/base:1.3"] = pin("1.3.2", "base")
			}))
			after, err := fsys.Stat(LockPath(root))
			testenv.Must(t, err)
			if _, onDisk := fsys.(*afero.OsFs); onDisk && !os.SameFile(info, after) {
				t.Error("writing the same pins again replaced the lockfile")
			}
			again, err := afero.ReadFile(fsys, LockPath(root))
			testenv.Must(t, err)
			if string(again) != want {
				t.Errorf("writing the same pins again wrote\n%s\nwant\n%s", again, want)
			}

			l, err := ReadLock(fsys, root)
			testenv.Must(t, err)
			if l.Version != LockVersion || len(l.Pulls) != len(pins) {
				t.Fatalf("read back version %d with %d pins, want version %d with %d", l.Version, len(l.Pulls),
					LockVersion, len(pins))
			}
			for image, p := range pins {
				if l.Pulls[image] != p {
					t.Errorf("%s: read back %+v, want %+v", image, l.Pulls[image], p)
				}
			}
			matches, err := afero.Glob(fsys, root+"/.vamos.lock-*")
			testenv.Must(t, err)
			if len(matches) > 0 {
				t.Errorf("left temporary files behind: %q", matches)
			}
		})
	}
}

// TestLockKeepsEveryWritersPins checks that writers that update the lockfile at once, as builds recording different
// pins do, each keep their pins, since each holds the workspace's lock while it reads the lockfile again and writes
// it. On disk the lock is an flock, which separate opens of the root contend for, as separate processes do.
func TestLockKeepsEveryWritersPins(t *testing.T) {
	for name, open := range lockFilesystems(t) {
		t.Run(name, func(t *testing.T) {
			fsys, root := open(t)
			want := map[string]Pin{}
			for i := range 20 {
				want[fmt.Sprintf("docker.io/library/app%d:1", i)] = pin("1", fmt.Sprint(i))
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			for image, p := range want {
				wg.Go(func() {
					<-start
					testenv.Must(t, updateLock(t.Context(), fsys, root, func(l *Lock) {
						runtime.Gosched() // so that another writer would read the same lockfile, without the lock
						l.Pulls[image] = p
					}))
				})
			}
			close(start)
			wg.Wait()

			l, err := ReadLock(fsys, root)
			testenv.Must(t, err)
			if !maps.Equal(l.Pulls, want) {
				t.Errorf("pinned %v, want %v", l.Pulls, want)
			}
		})
	}
}

// TestLockKeepsFieldsItDoesntKnow checks that rewriting a lockfile keeps the fields that this vamos doesn't know,
// as a newer one of the same version may write: the lockfile's, and those of each pin that it doesn't change,
// after the fields it knows, sorted; a pin that it changes loses them, since they may describe what it pinned before.
func TestLockKeepsFieldsItDoesntKnow(t *testing.T) {
	for name, open := range lockFilesystems(t) {
		t.Run(name, func(t *testing.T) {
			fsys, root := open(t)
			kept, changed := digest.FromString("kept"), digest.FromString("changed")
			testenv.Must(t, afero.WriteFile(fsys, LockPath(root), []byte(`{
  "version": 1,
  "zebra": [1, 2],
  "pulls": {
    "docker.io/library/a:1": {"tag": "1", "digest": "`+kept.String()+`", "platforms": ["linux/amd64"]},
    "docker.io/library/b:1": {"tag": "1", "digest": "`+changed.String()+`", "platforms": ["linux/arm64"]}
  },
  "alpha": {"b": 1}
}
`), 0o644))
			testenv.Must(t, updateLock(t.Context(), fsys, root, func(l *Lock) {
				l.Pulls["docker.io/library/b:1"] = pin("1.1", "new")
			}))
			want := `{
  "version": 1,
  "pulls": {
    "docker.io/library/a:1": {
      "tag": "1",
      "digest": "` + kept.String() + `",
      "platforms": [
        "linux/amd64"
      ]
    },
    "docker.io/library/b:1": {
      "tag": "1.1",
      "digest": "` + digest.FromString("new").String() + `"
    }
  },
  "alpha": {
    "b": 1
  },
  "zebra": [
    1,
    2
  ]
}
`
			got, err := afero.ReadFile(fsys, LockPath(root))
			testenv.Must(t, err)
			if string(got) != want {
				t.Errorf("wrote\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// TestLockWritesNothingForNoPins checks that a change that pins nothing leaves a workspace without a lockfile without
// one.
func TestLockWritesNothingForNoPins(t *testing.T) {
	for name, open := range lockFilesystems(t) {
		t.Run(name, func(t *testing.T) {
			fsys, root := open(t)
			testenv.Must(t, updateLock(t.Context(), fsys, root, func(*Lock) {}))
			if exists, err := afero.Exists(fsys, LockPath(root)); err != nil || exists {
				t.Errorf("a lockfile exists (%v), want none", err)
			}
		})
	}
}

// TestLoadPinsPulls checks that loading gives each pull the digest that the workspace root's lockfile pins its image
// and tag to, in every package, and none to a pull that it doesn't pin.
func TestLoadPinsPulls(t *testing.T) {
	fsys := testenv.Project(t, map[string]string{
		"WORKSPACE": "",
		"BUILD":     `pull(name="pinned", image="busybox", tag="1.36")`,
		"ui/BUILD": `
pull(name="pinned", image="docker.io/library/busybox", tag="1.36")
pull(name="unpinned", image="busybox", tag="1.37")
`,
	})
	pinned := pin("1.36.1", "busybox")
	testenv.Must(t, updateLock(t.Context(), fsys, testenv.ProjectDir, func(l *Lock) {
		l.Pulls["docker.io/library/busybox:1.36"] = pinned
	}))
	g, err := Load(t.Context(), fsys, Request{
		Dir:          testenv.ProjectDir + "/ui",
		Targets:      []string{"//:pinned", "pinned", "unpinned"},
		ExecPlatform: imgspecv1.Platform{OS: "linux", Architecture: "amd64"},
	})
	testenv.Must(t, err)
	want := map[string]digest.Digest{"//:pinned": pinned.Digest, "pinned": pinned.Digest, "unpinned": ""}
	for _, r := range g.Roots {
		n := g.Node(r)
		if n.Pull.Digest != want[n.Label] {
			t.Errorf("%s: pinned to %q, want %q", n.Label, n.Pull.Digest, want[n.Label])
		}
	}
}

// TestLockWaitEndsWithItsContext checks that a writer waiting for the workspace's lock, which another holds,
// stops waiting when its context ends, and writes nothing.
func TestLockWaitEndsWithItsContext(t *testing.T) {
	for name, open := range lockFilesystems(t) {
		t.Run(name, func(t *testing.T) {
			fsys, root := open(t)
			unlock, err := lockWorkspace(t.Context(), fsys, root)
			testenv.Must(t, err)
			defer unlock()
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err = updateLock(ctx, fsys, root, func(l *Lock) { l.Pulls["docker.io/library/a:1"] = pin("1", "a") })
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("UpdateLock returned %v, want the context's deadline", err)
			}
			if exists, err := afero.Exists(fsys, LockPath(root)); err != nil || exists {
				t.Errorf("a lockfile exists (%v), want none", err)
			}
		})
	}
}
