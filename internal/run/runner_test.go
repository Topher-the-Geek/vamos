package run

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/spf13/afero"
)

// memFile returns a filesystem holding the file /f with content.
func memFile(content string) afero.Fs {
	fsys := afero.NewMemMapFs()
	afero.WriteFile(fsys, "/f", []byte(content), 0o644)
	return fsys
}

// brokenContent is a content store that can't be reached: nothing in it can be checked or read.
type brokenContent struct {
	cache.ContentStore
	err error
}

// Has fails.
func (b *brokenContent) Has(context.Context, cache.Blob) error {
	return b.err
}

// Get fails.
func (b *brokenContent) Get(context.Context, cache.Blob) (io.ReadCloser, error) {
	return nil, b.err
}

// Read fails.
func (b *brokenContent) Read(context.Context, cache.Blob, int64) ([]byte, error) {
	return nil, b.err
}

// TestRunnerCheckContent checks that the local runner's check tells a task's files that are corrupt,
// which make its result unusable, from a content store that can't be reached, which is an error.
func TestRunnerCheckContent(t *testing.T) {
	m := cache.NewMemory()
	a := &sched.Action{Version: sched.ActionVersion, Kind: sched.KindTask, Output: fileset.DefaultSelection()}
	file, _, err := cache.PutFile(t.Context(), m, memFile("data"), "/f", 100)
	testenv.Must(t, err)
	tree, err := m.PutBytes(t.Context(), (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "f", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
	}}).Encode())
	testenv.Must(t, err)
	result := &cache.ActionResult{Output: &tree}

	r := &localRunner{content: m}
	if problem, err := r.Check(t.Context(), a, result); problem != nil || err != nil {
		t.Errorf("check = %v, %v; want the result usable", problem, err)
	}
	fire := errors.New("disk on fire")
	r.content = &brokenContent{ContentStore: m, err: fire}
	if problem, err := r.Check(t.Context(), a, result); problem != nil || !errors.Is(err, fire) {
		t.Errorf("check with the store unreachable = %v, %v; want only the store's error", problem, err)
	}
	m.Replace(file, nil)
	r.content = m
	if problem, err := r.Check(t.Context(), a, result); err != nil || problem == nil ||
		!strings.HasPrefix(problem.Error(), "its files can't be used from the cache: f: ") {
		t.Errorf("check with a file corrupt = %v, %v; want the result unusable", problem, err)
	}
}
