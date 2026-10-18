// Package cachetest tests implementations of cache.ContentStore and cache.RecordStore:
// Content and Records check the stores' contracts on any implementation, local or remote.
// It is for tests alone.
package cachetest

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// Fixture makes the stores that the suite tests.
type Fixture struct {
	// New returns fresh stores, which t's cleanup closes.
	New func(t *testing.T) Stores
}

// Stores is one implementation of the store interfaces, under test.
type Stores struct {
	Content cache.ContentStore
	Records cache.RecordStore // nil if the implementation has no record store; Records then skips
	// Unclearable says that the content store refuses ClearContent, as a remote one does, which a prune clears.
	Unclearable bool
	// Corrupt makes the content store hold data under b's digest, whatever data is, however the implementation keeps
	// blobs, so that the suite can corrupt a blob as a bad disk might.
	Corrupt func(t *testing.T, b cache.Blob, data []byte)
}

// must fails the test at once if err isn't nil.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Content checks the contract of a content store: blobs are found by digest, verified as they are read, put only
// within limits, imported only as advertised, dropped once found corrupt so that a put repairs them,
// and refused when malformed; and that trees are read back as stored and refused when invalid.
func Content(t *testing.T, f Fixture) {
	t.Run("blobs", func(t *testing.T) { testContentStore(t, f.New(t)) })
	t.Run("import", func(t *testing.T) { testImport(t, f.New(t)) })
	t.Run("corrupt", func(t *testing.T) { testCorrupt(t, f.New(t)) })
	t.Run("put file repairs", func(t *testing.T) { testPutFileRepairs(t, f.New(t)) })
	t.Run("malformed digests", func(t *testing.T) { testMalformedDigests(t, f.New(t)) })
	t.Run("trees", func(t *testing.T) { testReadTree(t, f.New(t)) })
	t.Run("checkout corrupt file", func(t *testing.T) { testCheckoutCorruptFile(t, f.New(t)) })
}

// Records checks the contract of a record store: the latest result for a key is kept, a key with none has no result,
// malformed records are refused, tag records are kept until replaced, and clearing results leaves tags.
func Records(t *testing.T, f Fixture) {
	s := f.New(t)
	if s.Records == nil {
		t.Skip("the implementation has no record store")
	}
	testRecordStore(t, s)
}

// testContentStore checks that blobs are found by digest, verified as they are read, and put only within limits.
func testContentStore(t *testing.T, s Stores) {
	ctx := t.Context()
	data := []byte("some content")
	b, err := s.Content.PutBytes(ctx, data)
	must(t, err)
	if want := (cache.Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}); b != want {
		t.Errorf("PutBytes = %+v, want %+v", b, want)
	}
	if err := s.Content.Has(ctx, b); err != nil {
		t.Errorf("Has = %v, want it held", err)
	}
	got, err := s.Content.Read(ctx, b, int64(len(data)))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("Read = %q, %v; want %q", got, err, data)
	}
	if _, err := s.Content.Read(ctx, b, int64(len(data))-1); !errors.Is(err, cache.ErrOverLimit) {
		t.Errorf("Read with too small a limit = %v, want %v", err, cache.ErrOverLimit)
	}
	other := cache.Blob{Digest: digest.FromString("other"), Size: 5}
	if err := s.Content.Has(ctx, other); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("Has of a blob never put = %v, want %v", err, cache.ErrMissing)
	}
	if _, err := s.Content.Get(ctx, other); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("Get of a blob never put = %v, want %v", err, cache.ErrMissing)
	}
	missing, err := s.Content.Missing(ctx, []cache.Blob{b, other})
	if err != nil || len(missing) != 1 || missing[0] != other {
		t.Errorf("Missing = %+v, %v; want the blob never put", missing, err)
	}
	if missing, err := s.Content.Missing(ctx, nil); err != nil || len(missing) != 0 {
		t.Errorf("Missing of nothing = %+v, %v; want nothing", missing, err)
	}
	if _, err := s.Content.Put(ctx, bytes.NewReader(data), int64(len(data))-1); !errors.Is(err, cache.ErrOverLimit) {
		t.Errorf("Put over the limit = %v, want %v", err, cache.ErrOverLimit)
	}
	contents, err := s.Content.ReadMany(ctx, []cache.Blob{b, b}, int64(len(data)))
	if err != nil || len(contents) != 2 || !bytes.Equal(contents[1], data) {
		t.Errorf("ReadMany = %q, %v; want the content twice", contents, err)
	}
	if _, err := s.Content.ReadMany(ctx, []cache.Blob{b, other}, int64(len(data))); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("ReadMany with a blob never put = %v, want %v", err, cache.ErrMissing)
	}
	if _, err := s.Content.ReadMany(ctx, []cache.Blob{b}, int64(len(data))-1); !errors.Is(err, cache.ErrOverLimit) {
		t.Errorf("ReadMany with too small a limit = %v, want %v", err, cache.ErrOverLimit)
	}
	empty, err := s.Content.PutBytes(ctx, nil)
	if err != nil || empty.Size != 0 {
		t.Errorf("PutBytes of nothing = %+v, %v; want the empty blob", empty, err)
	}
	if got, err := s.Content.Read(ctx, empty, 0); err != nil || len(got) != 0 {
		t.Errorf("Read of the empty blob = %q, %v; want nothing", got, err)
	}
	if s.Unclearable {
		if _, err := s.Content.ClearContent(ctx); err == nil {
			t.Error("ClearContent of a store that refuses it succeeded")
		}
		if err := s.Content.Has(ctx, b); err != nil {
			t.Errorf("Has after a refused clear = %v, want it held", err)
		}
		return
	}
	if n, err := s.Content.ClearContent(ctx); err != nil || n != 2 {
		t.Errorf("ClearContent = %d, %v; want 2 blobs removed", n, err)
	}
	if err := s.Content.Has(ctx, b); !errors.Is(err, cache.ErrMissing) {
		t.Errorf("Has after clearing = %v, want %v", err, cache.ErrMissing)
	}
	if _, err := s.Content.PutBytes(ctx, data); err != nil {
		t.Errorf("PutBytes after clearing: %v", err)
	}
}

// testImport checks that imported bytes become visible only if they are what was advertised.
func testImport(t *testing.T, s Stores) {
	ctx := t.Context()
	data := []byte("imported")
	b := cache.Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}
	for _, tc := range []struct {
		name string
		b    cache.Blob
		data []byte
	}{
		{name: "wrong content", b: b, data: []byte("imposter")},
		{name: "truncated", b: b, data: data[:4]},
		{name: "too long", b: b, data: append(append([]byte{}, data...), '!')},
		{name: "wrong size", b: cache.Blob{Digest: b.Digest, Size: b.Size + 1}, data: data},
	} {
		if err := s.Content.Import(ctx, tc.b, bytes.NewReader(tc.data)); !errors.Is(err, cache.ErrCorrupt) {
			t.Errorf("Import of %s = %v, want %v", tc.name, err, cache.ErrCorrupt)
		}
		if err := s.Content.Has(ctx, tc.b); !errors.Is(err, cache.ErrMissing) {
			t.Errorf("after importing %s, Has = %v, want %v", tc.name, err, cache.ErrMissing)
		}
	}
	must(t, s.Content.Import(ctx, b, bytes.NewReader(data)))
	if got, err := s.Content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
		t.Errorf("Read after importing = %q, %v", got, err)
	}
}

// testCorrupt checks that content that doesn't match its digest is reported as corrupt when read,
// however it differs, and told apart from content that is missing;
// that a blob found corrupt is dropped, so that putting its content again repairs it, though its size was right,
// as cache.PutFile does for sources captured again; and that a reader that finds corrupt a blob repaired since
// leaves the repair.
func testCorrupt(t *testing.T, s Stores) {
	ctx := t.Context()
	data := []byte("the real content")
	b, err := s.Content.PutBytes(ctx, data)
	must(t, err)
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
			s.Corrupt(t, b, tc.data)
			if err := s.Content.Has(ctx, b); tc.hasDetect && !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("Has = %v, want %v, since the size differs", err, cache.ErrCorrupt)
			} else if !tc.hasDetect && err != nil {
				t.Errorf("Has = %v, want nil, since the size is right", err)
			}
			if tc.hasDetect {
				if missing, err := s.Content.Missing(ctx, []cache.Blob{b}); err != nil || len(missing) != 1 {
					t.Errorf("Missing = %v, %v; want the corrupt blob", missing, err)
				}
			}
			if _, err := s.Content.Read(ctx, b, b.Size); !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("Read = %v, want %v", err, cache.ErrCorrupt)
			} else {
				var ce *cache.CorruptError
				if !errors.As(err, &ce) || ce.Blob != b {
					t.Errorf("Read's error %v doesn't name the blob", err)
				}
			}
			if errors.Is(err, cache.ErrMissing) {
				t.Errorf("Read reports corrupt content as missing: %v", err)
			}
			// Found corrupt, it is dropped, so that a client that has its content puts it again.
			if err := s.Content.Has(ctx, b); !errors.Is(err, cache.ErrMissing) {
				t.Errorf("Has after a read found it corrupt = %v, want %v", err, cache.ErrMissing)
			}
			if missing, err := s.Content.Missing(ctx, []cache.Blob{b}); err != nil || len(missing) != 1 {
				t.Errorf("Missing after a read found it corrupt = %v, %v; want the blob", missing, err)
			}

			s.Corrupt(t, b, tc.data)
			r, err := s.Content.Get(ctx, b)
			must(t, err)
			_, err = io.Copy(io.Discard, r)
			r.Close()
			if !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("reading Get to its end = %v, want %v", err, cache.ErrCorrupt)
			}

			// A reader that opened the corrupt blob before it was repaired leaves the repair.
			s.Corrupt(t, b, tc.data)
			r, err = s.Content.Get(ctx, b)
			must(t, err)
			if _, err := s.Content.PutBytes(ctx, data); err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, r)
			r.Close()
			if !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("reading to its end a Get opened before the repair = %v, want %v", err, cache.ErrCorrupt)
			}
			if got, err := s.Content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
				t.Errorf("Read after putting again = %q, %v", got, err)
			}
		})
	}
}

// testPutFileRepairs checks that capturing a file again repairs its blob once a read has found it corrupt,
// though the corrupt content has the right size, so that Has alone can't tell.
func testPutFileRepairs(t *testing.T, s Stores) {
	ctx := t.Context()
	fsys := afero.NewMemMapFs()
	data := []byte("the real content")
	must(t, afero.WriteFile(fsys, "/src.txt", data, 0o644))
	b, _, err := cache.PutFile(ctx, s.Content, fsys, "/src.txt", int64(len(data)))
	must(t, err)
	s.Corrupt(t, b, []byte("the fake content"))
	if _, err := s.Content.Read(ctx, b, b.Size); !errors.Is(err, cache.ErrCorrupt) {
		t.Fatalf("Read = %v, want %v", err, cache.ErrCorrupt)
	}
	if _, stored, err := cache.PutFile(ctx, s.Content, fsys, "/src.txt", int64(len(data))); err != nil || !stored {
		t.Errorf("PutFile again = stored %v, %v; want the content stored again", stored, err)
	}
	if got, err := s.Content.Read(ctx, b, b.Size); err != nil || !bytes.Equal(got, data) {
		t.Errorf("Read after capturing again = %q, %v; want the real content", got, err)
	}
}

// testMalformedDigests checks that a digest that isn't one is refused by every method, before it could name a path.
func testMalformedDigests(t *testing.T, s Stores) {
	ctx := t.Context()
	for _, d := range []digest.Digest{"", "sha256:../../etc/passwd", "sha256:abc",
		digest.Digest("md5:" + strings.Repeat("0", 32)), digest.Digest("sha512:" + strings.Repeat("0", 128)),
		digest.Digest(strings.Repeat("0", 64))} {
		b := cache.Blob{Digest: d, Size: 1}
		if err := s.Content.Has(ctx, b); err == nil || errors.Is(err, cache.ErrMissing) {
			t.Errorf("Has(%q) = %v, want it refused as malformed", d, err)
		}
		if _, err := s.Content.Get(ctx, b); err == nil || errors.Is(err, cache.ErrMissing) {
			t.Errorf("Get(%q) = %v, want it refused as malformed", d, err)
		}
		if _, err := s.Content.Read(ctx, b, 1); err == nil || errors.Is(err, cache.ErrMissing) {
			t.Errorf("Read(%q) = %v, want it refused as malformed", d, err)
		}
		if err := s.Content.Import(ctx, b, strings.NewReader("x")); err == nil {
			t.Errorf("Import(%q) succeeded, want it refused as malformed", d)
		}
		if _, err := s.Content.Missing(ctx, []cache.Blob{b}); err == nil {
			t.Errorf("Missing(%q) succeeded, want it refused as malformed", d)
		}
	}
	negative := cache.Blob{Digest: digest.FromString("x"), Size: -1}
	if err := s.Content.Has(ctx, negative); err == nil || errors.Is(err, cache.ErrMissing) {
		t.Errorf("Has of a blob with a negative size = %v, want it refused", err)
	}
}

// testReadTree checks that a tree is read back as stored, and that one that isn't a valid tree,
// or is too large, is refused as corrupt.
func testReadTree(t *testing.T, s Stores) {
	ctx := t.Context()
	a, err := s.Content.PutBytes(ctx, []byte("a"))
	must(t, err)
	good := &cache.Tree{Entries: []cache.TreeEntry{
		{Path: "dir", Type: cache.EntryDir, Mode: 0o755},
		{Path: "dir/a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
		{Path: "link", Type: cache.EntryLink, Target: "dir/a.txt"},
	}}
	b, err := s.Content.PutBytes(ctx, good.Encode())
	must(t, err)
	got, err := cache.ReadTree(ctx, s.Content, b)
	if err != nil || len(got.Entries) != 3 {
		t.Errorf("ReadTree = %+v, %v; want the tree", got, err)
	}
	must(t, cache.CheckTree(ctx, s.Content, b))

	for _, tc := range []struct {
		name    string
		entries []cache.TreeEntry
		want    string
	}{
		{
			name:    "parent path",
			entries: []cache.TreeEntry{{Path: "../escaped", Type: cache.EntryLink, Target: "x"}},
			want:    `"../escaped" isn't a clean path`,
		},
		{
			name:    "absolute path",
			entries: []cache.TreeEntry{{Path: "/escaped", Type: cache.EntryLink, Target: "x"}},
			want:    `"/escaped" isn't a clean path`,
		},
		{
			name: "beneath a link",
			entries: []cache.TreeEntry{
				{Path: "out", Type: cache.EntryLink, Target: "/tmp"},
				{Path: "out/escaped", Type: cache.EntryLink, Target: "x"},
			},
			want: `"out/escaped" is beneath the symbolic link "out"`,
		},
		{
			name: "beneath a link named by an unclean path",
			entries: []cache.TreeEntry{
				{Path: "a/../out", Type: cache.EntryLink, Target: "/tmp"},
				{Path: "out/escaped", Type: cache.EntryLink, Target: "x"},
			},
			want: `"a/../out" isn't a clean path`,
		},
		{
			name:    "file without content",
			entries: []cache.TreeEntry{{Path: "empty", Type: cache.EntryFile, Mode: 0o644}},
			want:    `file "empty" has no content`,
		},
		{
			name:    "unknown type",
			entries: []cache.TreeEntry{{Path: "dev", Type: "device", Mode: 0o644}},
			want:    `"dev" has unknown type "device"`,
		},
		{
			name:    "duplicate path",
			entries: []cache.TreeEntry{{Path: "a", Type: cache.EntryDir}, {Path: "a", Type: cache.EntryDir}},
			want:    `"a" isn't after "a"`,
		},
		{
			name:    "unsorted",
			entries: []cache.TreeEntry{{Path: "b", Type: cache.EntryDir}, {Path: "a", Type: cache.EntryDir}},
			want:    `"a" isn't after "b"`,
		},
		{
			name:    "mode beyond chmod bits",
			entries: []cache.TreeEntry{{Path: "a", Type: cache.EntryDir, Mode: 0o10000}},
			want:    `"a" has mode 10000`,
		},
		{
			name:    "link with content",
			entries: []cache.TreeEntry{{Path: "l", Type: cache.EntryLink, Target: "x", Blob: &a}},
			want:    `link "l" has a mode or content`,
		},
		{
			name: "file with malformed digest",
			entries: []cache.TreeEntry{{Path: "f", Type: cache.EntryFile,
				Blob: &cache.Blob{Digest: "sha256:../x", Size: 1}}},
			want: `file "f": blob "sha256:../x"`,
		},
		{
			name:    "empty path",
			entries: []cache.TreeEntry{{Path: "", Type: cache.EntryDir}},
			want:    `entry 0 has no path`,
		},
		{
			name: "beneath a file",
			entries: []cache.TreeEntry{
				{Path: "f", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
				{Path: "f/x", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
			},
			want: `"f/x" is beneath the file "f"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := s.Content.PutBytes(ctx, (&cache.Tree{Entries: tc.entries}).Encode())
			must(t, err)
			_, err = cache.ReadTree(ctx, s.Content, b)
			if !errors.Is(err, cache.ErrCorrupt) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ReadTree = %v, want %v containing %q", err, cache.ErrCorrupt, tc.want)
			}
			if _, err := cache.Checkout(ctx, s.Content, b, t.TempDir()); !errors.Is(err, cache.ErrCorrupt) {
				t.Errorf("Checkout = %v, want %v", err, cache.ErrCorrupt)
			}
		})
	}
	t.Run("not JSON", func(t *testing.T) {
		b, err := s.Content.PutBytes(ctx, []byte("{"))
		must(t, err)
		if _, err := cache.ReadTree(ctx, s.Content, b); !errors.Is(err, cache.ErrCorrupt) {
			t.Errorf("ReadTree = %v, want %v", err, cache.ErrCorrupt)
		}
	})
	t.Run("too large", func(t *testing.T) {
		huge := cache.Blob{Digest: digest.FromString("huge"), Size: cache.MaxTreeSize + 1}
		if _, err := cache.ReadTree(ctx, s.Content, huge); !errors.Is(err, cache.ErrOverLimit) {
			t.Errorf("ReadTree = %v, want %v", err, cache.ErrOverLimit)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		gone := cache.Blob{Digest: digest.FromString("gone"), Size: 4}
		b, err := s.Content.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
			{Path: "gone", Type: cache.EntryFile, Mode: 0o644, Blob: &gone},
		}}).Encode())
		must(t, err)
		if err := cache.CheckTree(ctx, s.Content, b); !errors.Is(err, cache.ErrMissing) {
			t.Errorf("CheckTree = %v, want %v", err, cache.ErrMissing)
		}
		if _, err := cache.Checkout(ctx, s.Content, b, t.TempDir()); !errors.Is(err, cache.ErrMissing) {
			t.Errorf("Checkout = %v, want %v", err, cache.ErrMissing)
		}
	})
}

// testCheckoutCorruptFile checks that a checkout fails as corrupt when a file's content doesn't match its blob.
func testCheckoutCorruptFile(t *testing.T, s Stores) {
	ctx := t.Context()
	a, err := s.Content.PutBytes(ctx, []byte("real"))
	must(t, err)
	b, err := s.Content.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &a},
	}}).Encode())
	must(t, err)
	s.Corrupt(t, a, []byte("fake"))
	must(t, cache.CheckTree(ctx, s.Content, b)) // the size is right, so only reading tells
	dir := t.TempDir()
	_, err = cache.Checkout(ctx, s.Content, b, dir)
	var ce *cache.CorruptError
	if !errors.As(err, &ce) || ce.Blob != a {
		t.Errorf("Checkout = %v, want %v naming %s", err, cache.ErrCorrupt, a.Digest)
	}
}

// testRecordStore checks the contract of a record store.
func testRecordStore(t *testing.T, s Stores) {
	ctx := t.Context()
	key := digest.FromString("an action")
	if r, err := s.Records.Result(ctx, key); err != nil || r != nil {
		t.Fatalf("result before any = %v, %v; want none", r, err)
	}
	must(t, s.Records.SetResult(ctx, key, &cache.ActionResult{Epoch: 1}))
	must(t, s.Records.SetResult(ctx, key, &cache.ActionResult{Epoch: 2}))
	if r, err := s.Records.Result(ctx, key); err != nil || r == nil || r.Epoch != 2 {
		t.Errorf("result = %+v, %v; want the latest, with epoch 2", r, err)
	}
	bad := &cache.ActionResult{Image: "sha256:../../escape", Epoch: 3}
	if err := s.Records.SetResult(ctx, key, bad); err == nil {
		t.Error("SetResult accepted a result naming a malformed image digest")
	}
	if _, err := s.Records.Result(ctx, "not a digest"); err == nil {
		t.Error("Result accepted a malformed key")
	}
	if err := s.Records.SetResult(ctx, digest.Digest("sha512:"+strings.Repeat("0", 128)),
		&cache.ActionResult{Epoch: 1}); err == nil {
		t.Error("SetResult accepted a key that isn't a sha256 digest")
	}

	if r, err := s.Records.Tag(ctx, "example.com/app:1"); err != nil || r != nil {
		t.Fatalf("tag before any = %v, %v; want none", r, err)
	}
	record := &cache.TagRecord{Digest: digest.FromString("index"), MIMEType: "application/json", Manifest: []byte("{}"),
		Instances: map[string]cache.TagInstance{"linux/arm64": {
			Digest:   digest.FromString("manifest"),
			Image:    digest.FromString("image"),
			Platform: "linux/arm64/v8",
			Epoch:    5,
			Locations: []cache.Location{{
				Kind:      cache.LocationRegistry,
				Reference: "example.com/app@" + digest.FromString("m").String(),
			}},
		}}}
	must(t, s.Records.SetTag(ctx, "example.com/app:1", record))
	if err := s.Records.SetTag(ctx, "example.com/app:1", &cache.TagRecord{Digest: "nope"}); err == nil {
		t.Error("SetTag accepted a record with a malformed digest")
	}
	bad = &cache.ActionResult{
		Image:     digest.FromString("i"),
		Locations: []cache.Location{{Kind: "cloud", Reference: "x"}},
	}
	if err := s.Records.SetResult(ctx, key, bad); err == nil {
		t.Error("SetResult accepted a result with a location of an unknown kind")
	}
	for _, instance := range []cache.TagInstance{
		{Digest: "nope"},
		{Digest: digest.FromString("m"), Image: "nope"},
		{Digest: digest.FromString("m"), Locations: []cache.Location{{Kind: cache.LocationStore}}},
	} {
		malformed := &cache.TagRecord{
			Digest:    digest.FromString("index"),
			Instances: map[string]cache.TagInstance{"linux/amd64": instance},
		}
		if err := s.Records.SetTag(ctx, "example.com/app:1", malformed); err == nil {
			t.Errorf("SetTag accepted a record with the malformed instance %+v", instance)
		}
	}
	if g, err := s.Records.Generation(ctx); err != nil || g != 0 {
		t.Errorf("generation before any clearing = %d, %v; want 0", g, err)
	}
	if n, err := s.Records.ClearResults(ctx); err != nil || n != 1 {
		t.Errorf("ClearResults = %d, %v; want 1 result removed", n, err)
	}
	if g, err := s.Records.Generation(ctx); err != nil || g != 1 {
		t.Errorf("generation after clearing = %d, %v; want 1", g, err)
	}
	if r, err := s.Records.Result(ctx, key); err != nil || r != nil {
		t.Errorf("result after clearing = %v, %v; want none", r, err)
	}
	r, err := s.Records.Tag(ctx, "example.com/app:1")
	if err != nil || r == nil || r.Digest != record.Digest || string(r.Manifest) != string(record.Manifest) ||
		len(r.Instances) != 1 || r.Instances["linux/arm64"].Image != record.Instances["linux/arm64"].Image ||
		len(r.Instances["linux/arm64"].Locations) != 1 {
		t.Errorf("tag after clearing = %+v, %v; want the record kept, %+v", r, err, record)
	}
	must(t, s.Records.SetResult(ctx, key, &cache.ActionResult{Epoch: 4}))
	if r, err := s.Records.Result(ctx, key); err != nil || r == nil || r.Epoch != 4 {
		t.Errorf("result set after clearing = %+v, %v; want epoch 4", r, err)
	}
}
