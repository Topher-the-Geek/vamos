package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/iotest"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/opencontainers/go-digest"
)

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

// TestTagRecordLegacyInstances checks that a tag record written before instances kept more than their manifest's
// digest still decodes, as incomplete instances, which a resolve completes, so that an upgrade keeps every tag pinned.
func TestTagRecordLegacyInstances(t *testing.T) {
	d := newTestDirectory(t)
	ctx := context.Background()
	index, image := digest.FromString("index"), digest.FromString("image")
	legacy := []byte(`{"digest":"` + index + `","instances":{"linux/arm64":"` + image + `"}}`)
	testenv.Must(t, os.WriteFile(d.tagPath("t"), legacy, 0o600))
	r, err := d.Tag(ctx, "t")
	testenv.Must(t, err)
	want := &TagRecord{Digest: index, Instances: map[string]TagInstance{"linux/arm64": {Digest: image}}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("legacy tag record = %+v, want %+v", r, want)
	}
	if r.Instances["linux/arm64"].Complete() {
		t.Error("an instance with only its manifest's digest counts as complete")
	}
	malformed := []byte(`{"digest":"` + index + `","instances":{"linux/arm64":"not a digest"}}`)
	testenv.Must(t, os.WriteFile(d.tagPath("t"), malformed, 0o600))
	if r, err := d.Tag(ctx, "t"); err != nil || r != nil {
		t.Errorf("legacy tag record with a malformed instance = %+v, %v; want none", r, err)
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
