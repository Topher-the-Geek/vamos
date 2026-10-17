package cache

import (
	"context"
	"fmt"
	"io"
	"io/fs"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// PutFile stores the file name in fsys as a blob in store, and reports whether the store lacked its content.
// It hashes the file first, and copies it only if the store lacks that content, or holds it corrupt,
// so a file the store already holds is read once and not written.
// The blob returned is always the content the store holds, even if the file changes as it is read.
// It fails with ErrOverLimit, storing nothing, if the file holds more than limit bytes,
// and reads at most one byte past the limit, so a file that grows as it is read can't fill the store.
func PutFile(ctx context.Context, store ContentStore, fsys afero.Fs, name string, limit int64) (Blob, bool, error) {
	b, err := HashFile(fsys, name, limit)
	if err != nil {
		return Blob{}, false, err
	}
	if store.Has(ctx, b) == nil {
		return b, false, nil
	}
	f, err := fsys.Open(name)
	if err != nil {
		return Blob{}, false, err
	}
	defer f.Close()
	b, err = store.Put(ctx, f, limit)
	return b, err == nil, err
}

// HashFile returns the blob that the file name in fsys would be stored as, storing nothing.
// It fails with ErrOverLimit if the file holds more than limit bytes, and reads at most one byte past the limit.
func HashFile(fsys afero.Fs, name string, limit int64) (Blob, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return Blob{}, err
	}
	defer f.Close()
	digester := digest.Canonical.Digester()
	size, err := io.Copy(digester.Hash(), limited(f, limit))
	if err != nil {
		return Blob{}, err
	}
	if size > limit {
		return Blob{}, fmt.Errorf("%w: %d", ErrOverLimit, limit)
	}
	return Blob{Digest: digester.Digest(), Size: size}, nil
}

// ChmodBits returns the permission and special bits of info's mode, as chmod takes them.
func ChmodBits(info fs.FileInfo) uint32 {
	mode := fileMode(info)
	bits := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// fileModeOf returns the fs.FileMode for chmod bits.
func fileModeOf(bits uint32) fs.FileMode {
	mode := fs.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if bits&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if bits&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// fileMode returns the permission and special bits of info's mode.
func fileMode(info fs.FileInfo) fs.FileMode {
	return info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}
