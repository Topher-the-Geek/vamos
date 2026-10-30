package run

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/spf13/afero"
	"go.podman.io/buildah"
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

// TestGiveSecrets checks that a run() step's secrets become buildah's secret mounts, each value in a file only this
// process can read, and that a secret without a value, as on a worker, which is never sent one, fails the step.
func TestGiveSecrets(t *testing.T) {
	st := &targetState{secrets: map[string][]byte{"token": []byte("hunter2")}}
	var options buildah.RunOptions
	dir, err := st.giveSecrets([]graph.Secret{{ID: "token", Env: "TOKEN"}, {ID: "token", File: "/run/t"}}, &options)
	defer os.RemoveAll(dir)
	testenv.Must(t, err)
	if want := []string{"type=secret,id=token,env=TOKEN", "type=secret,id=token,target=/run/t"}; !slices.Equal(
		options.RunMounts, want) {
		t.Errorf("mounts = %q, want %q", options.RunMounts, want)
	}
	source := options.Secrets["token"].Source
	if info, err := os.Stat(source); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the value's file %s: %v, %v; want it readable by its owner alone", source, info, err)
	}

	st.secrets = nil
	dir, err = st.giveSecrets([]graph.Secret{{ID: "token", Env: "TOKEN"}}, &buildah.RunOptions{})
	defer os.RemoveAll(dir)
	if err == nil || !strings.Contains(err.Error(), `secret "token" isn't given here`) {
		t.Errorf("a secret without a value = %v, want it refused", err)
	}
}

// TestBuilderOptionsNoHostMounts checks that a target's builder takes its mounts from an empty file,
// rather than the host's mounts.conf, whose mounts would make a target depend on the host.
func TestBuilderOptionsNoHostMounts(t *testing.T) {
	if got := builderOptions("scratch", nil, nil).DefaultMountsFilePath; got != os.DevNull {
		t.Errorf("the builder's mounts file = %q, want %s", got, os.DevNull)
	}
}
