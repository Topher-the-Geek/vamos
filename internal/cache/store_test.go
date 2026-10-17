package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// stores is one implementation of the store interfaces, under test.
type stores struct {
	content ContentStore
	records RecordStore
	// corrupt makes the store hold data under b's digest, however the implementation keeps blobs.
	corrupt func(t *testing.T, b Blob, data []byte)
}

// implementations returns each implementation of the stores, fresh, by name.
func implementations(t *testing.T) map[string]func(t *testing.T) stores {
	return map[string]func(t *testing.T) stores{
		"directory": func(t *testing.T) stores {
			d := newTestDirectory(t)
			return stores{content: d, records: d, corrupt: func(t *testing.T, b Blob, data []byte) {
				testenv.Must(t, os.WriteFile(d.blobPath(b), data, 0o600))
			}}
		},
		"memory": func(t *testing.T) stores {
			m := NewMemory()
			return stores{content: m, records: m, corrupt: func(t *testing.T, b Blob, data []byte) {
				m.Replace(b, data)
			}}
		},
	}
}

// eachImplementation runs test on each implementation of the stores.
func eachImplementation(t *testing.T, test func(t *testing.T, s stores)) {
	for name, newStores := range implementations(t) {
		t.Run(name, func(t *testing.T) { test(t, newStores(t)) })
	}
}

// TestContentStore checks the contract of a content store: blobs are found by digest, verified as they are read,
// put only within limits, and imported only as advertised.
func TestContentStore(t *testing.T) {
	eachImplementation(t, func(t *testing.T, s stores) {
		ctx := t.Context()
		data := []byte("some content")
		b, err := s.content.PutBytes(ctx, data)
		testenv.Must(t, err)
		if want := (Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}); b != want {
			t.Errorf("PutBytes = %+v, want %+v", b, want)
		}
		if err := s.content.Has(ctx, b); err != nil {
			t.Errorf("Has = %v, want it held", err)
		}
		got, err := s.content.Read(ctx, b, int64(len(data)))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("Read = %q, %v; want %q", got, err, data)
		}
		if _, err := s.content.Read(ctx, b, int64(len(data))-1); !errors.Is(err, ErrOverLimit) {
			t.Errorf("Read with too small a limit = %v, want %v", err, ErrOverLimit)
		}
		other := Blob{Digest: digest.FromString("other"), Size: 5}
		if err := s.content.Has(ctx, other); !errors.Is(err, ErrMissing) {
			t.Errorf("Has of a blob never put = %v, want %v", err, ErrMissing)
		}
		if _, err := s.content.Get(ctx, other); !errors.Is(err, ErrMissing) {
			t.Errorf("Get of a blob never put = %v, want %v", err, ErrMissing)
		}
		missing, err := s.content.Missing(ctx, []Blob{b, other})
		if err != nil || len(missing) != 1 || missing[0] != other {
			t.Errorf("Missing = %+v, %v; want the blob never put", missing, err)
		}
		if _, err := s.content.Put(ctx, bytes.NewReader(data), int64(len(data))-1); !errors.Is(err, ErrOverLimit) {
			t.Errorf("Put over the limit = %v, want %v", err, ErrOverLimit)
		}
		contents, err := s.content.ReadMany(ctx, []Blob{b, b}, int64(len(data)))
		if err != nil || len(contents) != 2 || !bytes.Equal(contents[1], data) {
			t.Errorf("ReadMany = %q, %v; want the content twice", contents, err)
		}
		if n, err := s.content.ClearContent(ctx); err != nil || n != 1 {
			t.Errorf("ClearContent = %d, %v; want 1 blob removed", n, err)
		}
		if err := s.content.Has(ctx, b); !errors.Is(err, ErrMissing) {
			t.Errorf("Has after clearing = %v, want %v", err, ErrMissing)
		}
		if _, err := s.content.PutBytes(ctx, data); err != nil {
			t.Errorf("PutBytes after clearing: %v", err)
		}
	})
}

// TestContentStoreImport checks that imported bytes become visible only if they are what was advertised.
func TestContentStoreImport(t *testing.T) {
	eachImplementation(t, func(t *testing.T, s stores) {
		ctx := t.Context()
		data := []byte("imported")
		b := Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}
		for _, tc := range []struct {
			name string
			b    Blob
			data []byte
		}{
			{name: "wrong content", b: b, data: []byte("imposter")},
			{name: "truncated", b: b, data: data[:4]},
			{name: "too long", b: b, data: append(data, '!')},
			{name: "wrong size", b: Blob{Digest: b.Digest, Size: b.Size + 1}, data: data},
		} {
			if err := s.content.Import(ctx, tc.b, bytes.NewReader(tc.data)); !errors.Is(err, ErrCorrupt) {
				t.Errorf("Import of %s = %v, want %v", tc.name, err, ErrCorrupt)
			}
			if err := s.content.Has(ctx, tc.b); !errors.Is(err, ErrMissing) {
				t.Errorf("after importing %s, Has = %v, want %v", tc.name, err, ErrMissing)
			}
		}
		testenv.Must(t, s.content.Import(ctx, b, bytes.NewReader(data)))
		if got, err := s.content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
			t.Errorf("Read after importing = %q, %v", got, err)
		}
	})
}

// TestContentStoreCorrupt checks that content that doesn't match its digest is reported as corrupt when read,
// however it differs, and told apart from content that is missing;
// that a blob found corrupt is dropped, so that putting its content again repairs it, though its size was right,
// as PutFile does for sources captured again; and that a reader that finds corrupt a blob repaired since
// leaves the repair.
func TestContentStoreCorrupt(t *testing.T) {
	eachImplementation(t, func(t *testing.T, s stores) {
		ctx := t.Context()
		data := []byte("the real content")
		b, err := s.content.PutBytes(ctx, data)
		testenv.Must(t, err)
		for _, tc := range []struct {
			name      string
			data      []byte
			hasDetect bool // Has can tell by the size alone
		}{
			{name: "same size", data: []byte("the fake content"), hasDetect: false},
			{name: "truncated", data: data[:5], hasDetect: true},
			{name: "longer", data: append(append([]byte{}, data...), "..."...), hasDetect: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s.corrupt(t, b, tc.data)
				if err := s.content.Has(ctx, b); tc.hasDetect && !errors.Is(err, ErrCorrupt) {
					t.Errorf("Has = %v, want %v, since the size differs", err, ErrCorrupt)
				} else if !tc.hasDetect && err != nil {
					t.Errorf("Has = %v, want nil, since the size is right", err)
				}
				if tc.hasDetect {
					if missing, err := s.content.Missing(ctx, []Blob{b}); err != nil || len(missing) != 1 {
						t.Errorf("Missing = %v, %v; want the corrupt blob", missing, err)
					}
				}
				if _, err := s.content.Read(ctx, b, b.Size); !errors.Is(err, ErrCorrupt) {
					t.Errorf("Read = %v, want %v", err, ErrCorrupt)
				} else {
					var ce *CorruptError
					if !errors.As(err, &ce) || ce.Blob != b {
						t.Errorf("Read's error %v doesn't name the blob", err)
					}
				}
				if errors.Is(err, ErrMissing) {
					t.Errorf("Read reports corrupt content as missing: %v", err)
				}
				// Found corrupt, it is dropped, so that a client that has its content puts it again.
				if err := s.content.Has(ctx, b); !errors.Is(err, ErrMissing) {
					t.Errorf("Has after a read found it corrupt = %v, want %v", err, ErrMissing)
				}
				if missing, err := s.content.Missing(ctx, []Blob{b}); err != nil || len(missing) != 1 {
					t.Errorf("Missing after a read found it corrupt = %v, %v; want the blob", missing, err)
				}

				s.corrupt(t, b, tc.data)
				r, err := s.content.Get(ctx, b)
				testenv.Must(t, err)
				_, err = io.Copy(io.Discard, r)
				r.Close()
				if !errors.Is(err, ErrCorrupt) {
					t.Errorf("reading Get to its end = %v, want %v", err, ErrCorrupt)
				}

				// A reader that opened the corrupt blob before it was repaired leaves the repair.
				s.corrupt(t, b, tc.data)
				r, err = s.content.Get(ctx, b)
				testenv.Must(t, err)
				if _, err := s.content.PutBytes(ctx, data); err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, r)
				r.Close()
				if !errors.Is(err, ErrCorrupt) {
					t.Errorf("reading to its end a Get opened before the repair = %v, want %v", err, ErrCorrupt)
				}
				if got, err := s.content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
					t.Errorf("Read after putting again = %q, %v", got, err)
				}
			})
		}
	})
}

// TestPutFileRepairs checks that capturing a file again repairs its blob once a read has found it corrupt,
// though the corrupt content has the right size, so that Has alone can't tell.
func TestPutFileRepairs(t *testing.T) {
	eachImplementation(t, func(t *testing.T, s stores) {
		ctx := t.Context()
		fsys := afero.NewMemMapFs()
		data := []byte("the real content")
		testenv.Must(t, afero.WriteFile(fsys, "/src.txt", data, 0o644))
		b, _, err := PutFile(ctx, s.content, fsys, "/src.txt", int64(len(data)))
		testenv.Must(t, err)
		s.corrupt(t, b, []byte("the fake content"))
		if _, err := s.content.Read(ctx, b, b.Size); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Read = %v, want %v", err, ErrCorrupt)
		}
		if _, stored, err := PutFile(ctx, s.content, fsys, "/src.txt", int64(len(data))); err != nil || !stored {
			t.Errorf("PutFile again = stored %v, %v; want the content stored again", stored, err)
		}
		if got, err := s.content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
			t.Errorf("Read after capturing again = %q, %v; want the real content", got, err)
		}
	})
}

// TestVerifier checks that the verifying stream accepts a blob's content however its reader delivers it,
// and refuses content that differs, however it differs.
func TestVerifier(t *testing.T) {
	data := []byte("content that is read in several pieces")
	b := Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}
	for name, wrap := range map[string]func(io.Reader) io.Reader{
		"one byte at a time": iotest.OneByteReader,
		"half reads":         iotest.HalfReader,
		"data with EOF":      iotest.DataErrReader,
		"plain":              func(r io.Reader) io.Reader { return r },
	} {
		t.Run(name, func(t *testing.T) {
			got, err := io.ReadAll(verify(io.NopCloser(wrap(bytes.NewReader(data))), b, nil))
			if err != nil || !bytes.Equal(got, data) {
				t.Errorf("read %q, %v; want the content", got, err)
			}
			for _, bad := range [][]byte{data[:len(data)-1], append(append([]byte{}, data...), 'x'),
				bytes.ToUpper(data), {}} {
				_, err := io.ReadAll(verify(io.NopCloser(wrap(bytes.NewReader(bad))), b, nil))
				if !errors.Is(err, ErrCorrupt) {
					t.Errorf("reading %q as the blob = %v, want %v", bad, err, ErrCorrupt)
				}
			}
		})
	}
}

// TestContentStoreMalformedDigests checks that a digest that isn't one is refused by every method,
// before it could name a path.
func TestContentStoreMalformedDigests(t *testing.T) {
	eachImplementation(t, func(t *testing.T, s stores) {
		ctx := t.Context()
		for _, d := range []digest.Digest{"", "sha256:../../etc/passwd", "sha256:abc",
			digest.Digest("md5:" + strings.Repeat("0", 32)), digest.Digest("sha512:" + strings.Repeat("0", 128)),
			digest.Digest(strings.Repeat("0", 64))} {
			b := Blob{Digest: d, Size: 1}
			if err := s.content.Has(ctx, b); err == nil || errors.Is(err, ErrMissing) {
				t.Errorf("Has(%q) = %v, want it refused as malformed", d, err)
			}
			if _, err := s.content.Get(ctx, b); err == nil || errors.Is(err, ErrMissing) {
				t.Errorf("Get(%q) = %v, want it refused as malformed", d, err)
			}
			if _, err := s.content.Read(ctx, b, 1); err == nil || errors.Is(err, ErrMissing) {
				t.Errorf("Read(%q) = %v, want it refused as malformed", d, err)
			}
			if err := s.content.Import(ctx, b, strings.NewReader("x")); err == nil {
				t.Errorf("Import(%q) succeeded, want it refused as malformed", d)
			}
			if _, err := s.content.Missing(ctx, []Blob{b}); err == nil {
				t.Errorf("Missing(%q) succeeded, want it refused as malformed", d)
			}
		}
		negative := Blob{Digest: digest.FromString("x"), Size: -1}
		if err := s.content.Has(ctx, negative); err == nil || errors.Is(err, ErrMissing) {
			t.Errorf("Has of a blob with a negative size = %v, want it refused", err)
		}
	})
}

// TestReadTree checks that a tree is read back as stored, and that one that isn't a valid tree,
// or is too large, is refused as corrupt.
func TestReadTree(t *testing.T) {
	eachImplementation(t, func(t *testing.T, s stores) {
		ctx := t.Context()
		a, err := s.content.PutBytes(ctx, []byte("a"))
		testenv.Must(t, err)
		good := &Tree{Entries: []TreeEntry{
			{Path: "dir", Type: EntryDir, Mode: 0o755},
			{Path: "dir/a.txt", Type: EntryFile, Mode: 0o644, Blob: &a},
			{Path: "link", Type: EntryLink, Target: "dir/a.txt"},
		}}
		b, err := s.content.PutBytes(ctx, good.Encode())
		testenv.Must(t, err)
		got, err := ReadTree(ctx, s.content, b)
		if err != nil || len(got.Entries) != 3 {
			t.Errorf("ReadTree = %+v, %v; want the tree", got, err)
		}
		testenv.Must(t, CheckTree(ctx, s.content, b))

		for _, tc := range []struct {
			name    string
			entries []TreeEntry
			want    string
		}{
			{
				name:    "parent path",
				entries: []TreeEntry{{Path: "../escaped", Type: EntryLink, Target: "x"}},
				want:    `"../escaped" isn't a clean path`,
			},
			{
				name:    "absolute path",
				entries: []TreeEntry{{Path: "/escaped", Type: EntryLink, Target: "x"}},
				want:    `"/escaped" isn't a clean path`,
			},
			{
				name: "beneath a link",
				entries: []TreeEntry{
					{Path: "out", Type: EntryLink, Target: "/tmp"},
					{Path: "out/escaped", Type: EntryLink, Target: "x"},
				},
				want: `"out/escaped" is beneath the symbolic link "out"`,
			},
			{
				name: "beneath a link named by an unclean path",
				entries: []TreeEntry{
					{Path: "a/../out", Type: EntryLink, Target: "/tmp"},
					{Path: "out/escaped", Type: EntryLink, Target: "x"},
				},
				want: `"a/../out" isn't a clean path`,
			},
			{
				name:    "file without content",
				entries: []TreeEntry{{Path: "empty", Type: EntryFile, Mode: 0o644}},
				want:    `file "empty" has no content`,
			},
			{
				name:    "unknown type",
				entries: []TreeEntry{{Path: "dev", Type: "device", Mode: 0o644}},
				want:    `"dev" has unknown type "device"`,
			},
			{
				name:    "duplicate path",
				entries: []TreeEntry{{Path: "a", Type: EntryDir}, {Path: "a", Type: EntryDir}},
				want:    `"a" isn't after "a"`,
			},
			{
				name:    "unsorted",
				entries: []TreeEntry{{Path: "b", Type: EntryDir}, {Path: "a", Type: EntryDir}},
				want:    `"a" isn't after "b"`,
			},
			{
				name:    "mode beyond chmod bits",
				entries: []TreeEntry{{Path: "a", Type: EntryDir, Mode: 0o10000}},
				want:    `"a" has mode 10000`,
			},
			{
				name:    "link with content",
				entries: []TreeEntry{{Path: "l", Type: EntryLink, Target: "x", Blob: &a}},
				want:    `link "l" has a mode or content`,
			},
			{
				name:    "file with malformed digest",
				entries: []TreeEntry{{Path: "f", Type: EntryFile, Blob: &Blob{Digest: "sha256:../x", Size: 1}}},
				want:    `file "f": blob "sha256:../x"`,
			},
			{
				name:    "empty path",
				entries: []TreeEntry{{Path: "", Type: EntryDir}},
				want:    `entry 0 has no path`,
			},
			{
				name: "beneath a file",
				entries: []TreeEntry{
					{Path: "f", Type: EntryFile, Mode: 0o644, Blob: &a},
					{Path: "f/x", Type: EntryFile, Mode: 0o644, Blob: &a},
				},
				want: `"f/x" is beneath the file "f"`,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b, err := s.content.PutBytes(ctx, (&Tree{Entries: tc.entries}).Encode())
				testenv.Must(t, err)
				_, err = ReadTree(ctx, s.content, b)
				if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("ReadTree = %v, want %v containing %q", err, ErrCorrupt, tc.want)
				}
				if _, err := Checkout(ctx, s.content, b, t.TempDir()); !errors.Is(err, ErrCorrupt) {
					t.Errorf("Checkout = %v, want %v", err, ErrCorrupt)
				}
			})
		}
		t.Run("record with another algorithm", func(t *testing.T) {
			key := digest.Digest("sha512:" + strings.Repeat("0", 128))
			if err := s.records.SetResult(ctx, key, &ActionResult{Epoch: 1}); err == nil {
				t.Error("SetResult accepted a key that isn't a sha256 digest")
			}
		})
		t.Run("not JSON", func(t *testing.T) {
			b, err := s.content.PutBytes(ctx, []byte("{"))
			testenv.Must(t, err)
			if _, err := ReadTree(ctx, s.content, b); !errors.Is(err, ErrCorrupt) {
				t.Errorf("ReadTree = %v, want %v", err, ErrCorrupt)
			}
		})
		t.Run("too large", func(t *testing.T) {
			huge := Blob{Digest: digest.FromString("huge"), Size: MaxTreeSize + 1}
			if _, err := ReadTree(ctx, s.content, huge); !errors.Is(err, ErrOverLimit) {
				t.Errorf("ReadTree = %v, want %v", err, ErrOverLimit)
			}
		})
		t.Run("missing file", func(t *testing.T) {
			gone := Blob{Digest: digest.FromString("gone"), Size: 4}
			b, err := s.content.PutBytes(ctx, (&Tree{Entries: []TreeEntry{
				{Path: "gone", Type: EntryFile, Mode: 0o644, Blob: &gone},
			}}).Encode())
			testenv.Must(t, err)
			if err := CheckTree(ctx, s.content, b); !errors.Is(err, ErrMissing) {
				t.Errorf("CheckTree = %v, want %v", err, ErrMissing)
			}
			if _, err := Checkout(ctx, s.content, b, t.TempDir()); !errors.Is(err, ErrMissing) {
				t.Errorf("Checkout = %v, want %v", err, ErrMissing)
			}
		})
	})
}

// TestCheckoutCorruptFile checks that a checkout fails as corrupt when a file's content doesn't match its blob.
func TestCheckoutCorruptFile(t *testing.T) {
	eachImplementation(t, func(t *testing.T, s stores) {
		ctx := t.Context()
		a, err := s.content.PutBytes(ctx, []byte("real"))
		testenv.Must(t, err)
		b, err := s.content.PutBytes(ctx, (&Tree{Entries: []TreeEntry{
			{Path: "a.txt", Type: EntryFile, Mode: 0o644, Blob: &a},
		}}).Encode())
		testenv.Must(t, err)
		s.corrupt(t, a, []byte("fake"))
		testenv.Must(t, CheckTree(ctx, s.content, b)) // the size is right, so only reading tells
		dir := t.TempDir()
		_, err = Checkout(ctx, s.content, b, dir)
		var ce *CorruptError
		if !errors.As(err, &ce) || ce.Blob != a {
			t.Errorf("Checkout = %v, want %v naming %s", err, ErrCorrupt, a.Digest)
		}
	})
}

// TestRecordStore checks the contract of a record store: the latest result for a key is kept,
// a key with none, or with one that can't be read, has no result, tag records are kept until replaced,
// and clearing results leaves tags.
func TestRecordStore(t *testing.T) {
	eachImplementation(t, func(t *testing.T, s stores) {
		ctx := t.Context()
		key := digest.FromString("an action")
		if r, err := s.records.Result(ctx, key); err != nil || r != nil {
			t.Fatalf("result before any = %v, %v; want none", r, err)
		}
		testenv.Must(t, s.records.SetResult(ctx, key, &ActionResult{Epoch: 1}))
		testenv.Must(t, s.records.SetResult(ctx, key, &ActionResult{Epoch: 2}))
		if r, err := s.records.Result(ctx, key); err != nil || r == nil || r.Epoch != 2 {
			t.Errorf("result = %+v, %v; want the latest, with epoch 2", r, err)
		}
		bad := &ActionResult{Image: "sha256:../../escape", Epoch: 3}
		if err := s.records.SetResult(ctx, key, bad); err == nil {
			t.Error("SetResult accepted a result naming a malformed image digest")
		}
		if _, err := s.records.Result(ctx, "not a digest"); err == nil {
			t.Error("Result accepted a malformed key")
		}

		if r, err := s.records.Tag(ctx, "example.com/app:1"); err != nil || r != nil {
			t.Fatalf("tag before any = %v, %v; want none", r, err)
		}
		record := &TagRecord{Digest: digest.FromString("index"), MIMEType: "application/json", Manifest: []byte("{}"),
			Instances: map[string]digest.Digest{"linux/arm64": digest.FromString("image")}}
		testenv.Must(t, s.records.SetTag(ctx, "example.com/app:1", record))
		if err := s.records.SetTag(ctx, "example.com/app:1", &TagRecord{Digest: "nope"}); err == nil {
			t.Error("SetTag accepted a record with a malformed digest")
		}
		if n, err := s.records.ClearResults(ctx); err != nil || n != 1 {
			t.Errorf("ClearResults = %d, %v; want 1 result removed", n, err)
		}
		if r, err := s.records.Result(ctx, key); err != nil || r != nil {
			t.Errorf("result after clearing = %v, %v; want none", r, err)
		}
		if r, err := s.records.Tag(ctx, "example.com/app:1"); err != nil || r == nil || r.Digest != record.Digest ||
			r.Instances["linux/arm64"] != record.Instances["linux/arm64"] {
			t.Errorf("tag after clearing = %+v, %v; want the record kept", r, err)
		}
		testenv.Must(t, s.records.SetResult(ctx, key, &ActionResult{Epoch: 4}))
		if r, err := s.records.Result(ctx, key); err != nil || r == nil || r.Epoch != 4 {
			t.Errorf("result set after clearing = %+v, %v; want epoch 4", r, err)
		}
	})
}

// TestDirectoryUnreadableRecords checks that a record on disk that can't be decoded, or is too large,
// is as good as none.
func TestDirectoryUnreadableRecords(t *testing.T) {
	d := newTestDirectory(t)
	ctx := context.Background()
	key := digest.FromString("an action")
	testenv.Must(t, os.WriteFile(d.actionPath(key), []byte("{"), 0o600))
	if r, err := d.Result(ctx, key); err != nil || r != nil {
		t.Errorf("unreadable result = %v, %v; want none", r, err)
	}
	testenv.Must(t, os.WriteFile(d.actionPath(key), []byte(`{"image":"sha256:../x"}`), 0o600))
	if r, err := d.Result(ctx, key); err != nil || r != nil {
		t.Errorf("result naming a malformed digest = %v, %v; want none", r, err)
	}
	huge := append([]byte(`{"epoch":1,"pad":"`), bytes.Repeat([]byte("x"), MaxRecordSize)...)
	testenv.Must(t, os.WriteFile(d.actionPath(key), append(huge, '"', '}'), 0o600))
	if r, err := d.Result(ctx, key); err != nil || r != nil {
		t.Errorf("oversized result = %v, %v; want none", r, err)
	}
	testenv.Must(t, os.WriteFile(d.tagPath("t"), []byte("nonsense"), 0o600))
	if r, err := d.Tag(ctx, "t"); err != nil || r != nil {
		t.Errorf("unreadable tag = %v, %v; want none", r, err)
	}
}

// TestDirectoryClearKeepsWorking checks that the directory can be used again after clearing it in one process.
func TestDirectoryClearKeepsWorking(t *testing.T) {
	d := newTestDirectory(t)
	ctx := context.Background()
	if _, err := d.PutBytes(ctx, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClearContent(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClearResults(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PutBytes(ctx, []byte("y")); err != nil {
		t.Errorf("PutBytes after clearing: %v", err)
	}
	if err := d.SetResult(ctx, digest.FromString("k"), &ActionResult{Epoch: 1}); err != nil {
		t.Errorf("SetResult after clearing: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(d.dir, "tmp")); err != nil || len(entries) != 0 {
		t.Errorf("tmp after clearing = %v, %v; want empty", entries, err)
	}
}
