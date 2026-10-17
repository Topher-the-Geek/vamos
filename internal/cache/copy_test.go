package cache

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/opencontainers/go-digest"
)

// TestCopy checks that Copy moves only the blobs the destination lacks, verified, and that a blob corrupt
// at the source neither lands at the destination nor stops the count from saying how far the copy got.
func TestCopy(t *testing.T) {
	ctx := t.Context()
	from, to := NewMemory(), NewMemory()
	var blobs []Blob
	for _, content := range []string{"one", "two", "three"} {
		b, err := from.PutBytes(ctx, []byte(content))
		testenv.Must(t, err)
		blobs = append(blobs, b)
	}
	// The destination has the first already.
	_, err := to.PutBytes(ctx, []byte("one"))
	testenv.Must(t, err)

	copied, err := Copy(ctx, from, to, blobs)
	if err != nil || copied != 2 {
		t.Errorf("Copy = %d, %v; want 2 blobs copied", copied, err)
	}
	if missing, err := to.Missing(ctx, blobs); err != nil || len(missing) != 0 {
		t.Errorf("after copying, the destination lacks %v, %v", missing, err)
	}
	if data, err := to.Read(ctx, blobs[2], blobs[2].Size); err != nil || !bytes.Equal(data, []byte("three")) {
		t.Errorf("the copied blob reads %q, %v", data, err)
	}
	if copied, err := Copy(ctx, from, to, blobs); err != nil || copied != 0 {
		t.Errorf("Copy again = %d, %v; want nothing copied", copied, err)
	}

	// A corrupt source blob fails the copy as corrupt, and the destination doesn't get it.
	four, err := from.PutBytes(ctx, []byte("four"))
	testenv.Must(t, err)
	from.Replace(four, []byte("faux"))
	gone := Blob{Digest: digest.FromString("gone"), Size: 4}
	copied, err = Copy(ctx, from, to, []Blob{four, gone})
	if !errors.Is(err, ErrCorrupt) || copied != 0 {
		t.Errorf("Copy of a corrupt blob = %d, %v; want %v after none copied", copied, err, ErrCorrupt)
	}
	if err := to.Has(ctx, four); !errors.Is(err, ErrMissing) {
		t.Errorf("the destination has the corrupt blob: %v", err)
	}
	if _, err := Copy(ctx, from, to, []Blob{gone}); !errors.Is(err, ErrMissing) {
		t.Errorf("Copy of a blob the source lacks = %v, want %v", err, ErrMissing)
	}
}
