package cache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// TestCancelCheckoutAndReplay checks that checking out a tree, and replaying a log, stop when the context ends,
// writing nothing.
func TestCancelCheckoutAndReplay(t *testing.T) {
	ctx := t.Context()
	store := NewMemory()
	file, err := store.PutBytes(ctx, []byte("x"))
	testenv.Must(t, err)
	var entries []TreeEntry
	for i := range 20 {
		entries = append(entries, TreeEntry{Path: "f" + string(rune('a'+i)), Type: EntryFile, Mode: 0o644, Blob: &file})
	}
	tree, err := store.PutBytes(ctx, (&Tree{Entries: entries}).Encode())
	testenv.Must(t, err)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	dir := t.TempDir()
	if _, err := Checkout(cancelled, store, tree, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("Checkout = %v, want %v", err, context.Canceled)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a cancelled checkout wrote %d files", len(entries))
	}

	log, err := NewLog()
	testenv.Must(t, err)
	defer log.Discard()
	log.Writer(StreamStdout, &bytes.Buffer{}).Write([]byte("hello\n"))
	saved, err := log.Save(ctx, store)
	testenv.Must(t, err)
	data, err := store.Read(ctx, *saved, MaxLogSize)
	testenv.Must(t, err)
	var out bytes.Buffer
	if err := ReplayLog(cancelled, bytes.NewReader(data), &out, &out); !errors.Is(err, context.Canceled) {
		t.Errorf("ReplayLog = %v, want %v", err, context.Canceled)
	}
	if out.Len() != 0 {
		t.Errorf("a cancelled replay wrote %q", out.String())
	}
}
