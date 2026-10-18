package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
)

// Directory is a content store and record store in a directory on disk: the local implementation of both,
// which the local executor keeps beside its container store.
// Independent writes publish complete files, including across processes;
// they do not deduplicate work or transact across records.
// An open Directory holds the directory's advisory lock shared, and Prune takes it exclusive,
// so that a prune and any other use exclude each other, across processes as within one;
// the unit of protection is the open handle, which Close releases.
// Both take the lock without waiting: a prune that meets an open handle, and an open that meets a prune,
// each fail with ErrLocked, naming the other.
type Directory struct {
	dir  string // holds cas/sha256/<hex>, ac/<hex>, tags/<hex>, tmp/ and the lock file
	lock *dirLock
}

var (
	_ ContentStore = (*Directory)(nil)
	_ RecordStore  = (*Directory)(nil)
)

// subdirectories are the directories a Directory holds.
var subdirectories = []string{"cas/sha256", "ac", "tags", "tmp"}

// OpenDirectory opens the stores in dir, creating it if need be, and holds its lock shared until Close.
// It fails with ErrLocked while a prune holds the directory.
func OpenDirectory(dir string) (*Directory, error) {
	if err := makeSubdirectories(dir); err != nil {
		return nil, fmt.Errorf("opening build cache: %w", err)
	}
	lock, err := lockShared(dir)
	if err != nil {
		return nil, fmt.Errorf("opening build cache: %w", err)
	}
	return &Directory{dir: dir, lock: lock}, nil
}

// Close releases the directory's lock.
// The stores can't be used afterwards.
func (d *Directory) Close() error {
	return d.lock.release()
}

// Prune removes every blob and action result, but no tag record, holding the directory's lock exclusive meanwhile,
// and returns how many results and blobs there were.
// first, if not nil, runs once the lock is held and before anything is removed, for what the caller removes
// under the same exclusion, such as the images the results name; its failure is reported with the prune's,
// which goes ahead.
// It fails with ErrLocked, removing nothing and without calling first, if another handle holds the directory,
// in this process or another.
func (d *Directory) Prune(ctx context.Context, first func() error) (results, blobs int, err error) {
	shared, err := d.lock.exclusive(d.dir)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if serr := shared(); serr != nil {
			err = errors.Join(err, fmt.Errorf("taking the build cache's lock back after pruning: %w", serr))
		}
	}()
	var errs []error
	if first != nil {
		if err := first(); err != nil {
			errs = append(errs, err)
		}
	}
	if results, err = d.ClearResults(ctx); err != nil {
		return results, 0, errors.Join(append(errs, err)...)
	}
	blobs, err = d.ClearContent(ctx)
	return results, blobs, errors.Join(append(errs, err)...)
}

// makeSubdirectories makes the directories a Directory holds within dir.
func makeSubdirectories(dir string) error {
	for _, sub := range subdirectories {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// blobPath returns where the blob b, which must be valid, is kept.
func (d *Directory) blobPath(b Blob) string {
	return filepath.Join(d.dir, "cas", b.Digest.Algorithm().String(), b.Digest.Encoded())
}

// pendingFile is a file being written into the directory.
// It is written to a temporary file and renamed into place once complete,
// so that it is never seen partly written, even by another vamos process.
// Its digest and size are worked out as it is written.
type pendingFile struct {
	f        *os.File
	digester digest.Digester
	size     int64
}

// create starts writing a file into the directory.
// The caller commits or discards it.
func (d *Directory) create() (*pendingFile, error) {
	f, err := os.CreateTemp(filepath.Join(d.dir, "tmp"), "pending-")
	if err != nil {
		return nil, err
	}
	return &pendingFile{f: f, digester: digest.Canonical.Digester()}, nil
}

// Write appends data to the file.
func (p *pendingFile) Write(data []byte) (int, error) {
	n, err := p.f.Write(data)
	p.digester.Hash().Write(data[:n])
	p.size += int64(n)
	return n, err
}

// blob returns the blob that what has been written so far makes.
func (p *pendingFile) blob() Blob {
	return Blob{Digest: p.digester.Digest(), Size: p.size}
}

// commit finishes the file and moves it to path, replacing whatever is there.
func (p *pendingFile) commit(path string) error {
	if err := p.f.Close(); err != nil {
		return err
	}
	return os.Rename(p.f.Name(), path)
}

// discard removes the file, unless it has been committed.
func (p *pendingFile) discard() {
	p.f.Close()
	os.Remove(p.f.Name()) // fails harmlessly once committed
}

// Has reports whether the directory holds b: a file of its size under its digest.
func (d *Directory) Has(_ context.Context, b Blob) error {
	if err := b.Validate(); err != nil {
		return err
	}
	info, err := os.Stat(d.blobPath(b))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return missing(b)
	case err != nil:
		return err
	case info.Size() != b.Size:
		return Corrupt(b, fmt.Sprintf("it holds %d bytes, not %d", info.Size(), b.Size))
	}
	return nil
}

// Missing returns which of blobs the directory lacks or holds corrupt.
func (d *Directory) Missing(ctx context.Context, blobs []Blob) ([]Blob, error) {
	return missingOf(ctx, d, blobs)
}

// Put stores what r holds as a blob, or fails with ErrOverLimit, storing nothing, if it holds more than limit bytes.
func (d *Directory) Put(_ context.Context, r io.Reader, limit int64) (Blob, error) {
	f, err := d.create()
	if err != nil {
		return Blob{}, err
	}
	defer f.discard()
	if _, err := io.Copy(f, limited(r, limit)); err != nil {
		return Blob{}, err
	}
	b := f.blob()
	if b.Size > limit {
		return Blob{}, fmt.Errorf("%w: %d", ErrOverLimit, limit)
	}
	// The same digest means the same content, so replacing an existing blob changes nothing, unless it was corrupt.
	return b, f.commit(d.blobPath(b))
}

// PutBytes stores data as a blob.
func (d *Directory) PutBytes(ctx context.Context, data []byte) (Blob, error) {
	return d.Put(ctx, bytes.NewReader(data), int64(len(data)))
}

// Import stores what r holds as b, failing with ErrCorrupt, and storing nothing, if it isn't b's content.
func (d *Directory) Import(_ context.Context, b Blob, r io.Reader) error {
	if err := b.Validate(); err != nil {
		return err
	}
	f, err := d.create()
	if err != nil {
		return err
	}
	defer f.discard()
	if _, err := io.Copy(f, limited(r, b.Size)); err != nil {
		return err
	}
	if got := f.blob(); got != b {
		return Corrupt(b, fmt.Sprintf("the bytes imported make blob %s of %d bytes", got.Digest, got.Size))
	}
	return f.commit(d.blobPath(b))
}

// Get opens b for reading, as a stream that verifies it.
func (d *Directory) Get(_ context.Context, b Blob) (io.ReadCloser, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	f, err := os.Open(d.blobPath(b))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, missing(b)
	} else if err != nil {
		return nil, err
	}
	return verify(f, b, func() { d.drop(b, f) }), nil
}

// drop removes the blob b, which f, open, was found to hold corrupt,
// unless the file under b's digest has been replaced since, as a put that repairs it does.
// A failure leaves the blob for a later read to find corrupt again.
func (d *Directory) drop(b Blob, f *os.File) {
	path := d.blobPath(b)
	opened, err := f.Stat()
	if err != nil {
		return
	}
	if current, err := os.Stat(path); err == nil && os.SameFile(opened, current) {
		os.Remove(path)
	}
}

// Read returns b's content, verified, if it is at most limit bytes.
func (d *Directory) Read(ctx context.Context, b Blob, limit int64) ([]byte, error) {
	return readAll(ctx, d, b, limit)
}

// ReadMany returns the content of each of blobs, verified.
func (d *Directory) ReadMany(ctx context.Context, blobs []Blob, limit int64) ([][]byte, error) {
	return readMany(ctx, d, blobs, limit)
}

// ClearContent removes every blob, and pending files, returning how many blobs there were.
// A writer in another process that is publishing meanwhile fails; see the type's contract.
func (d *Directory) ClearContent(context.Context) (int, error) {
	entries, err := os.ReadDir(filepath.Join(d.dir, "cas", "sha256"))
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, sub := range []string{"cas", "tmp"} {
		if err := os.RemoveAll(filepath.Join(d.dir, sub)); err != nil {
			errs = append(errs, err)
		}
	}
	return len(entries), errors.Join(append(errs, makeSubdirectories(d.dir))...)
}

// actionPath returns where the result for key, which must be valid, is kept.
func (d *Directory) actionPath(key digest.Digest) string {
	return filepath.Join(d.dir, "ac", key.Encoded())
}

// tagPath returns where the record of the tag name is kept.
func (d *Directory) tagPath(name string) string {
	return filepath.Join(d.dir, "tags", digest.FromString(name).Encoded())
}

// readRecord returns the record at path, decoded into v, which must be at most MaxRecordSize bytes,
// or false if there is none, or it can't be decoded, which is as good as none.
func readRecord(path string, v interface{ Validate() error }) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer f.Close()
	data, err := io.ReadAll(limited(f, MaxRecordSize))
	if err != nil {
		return false, err
	}
	if int64(len(data)) > MaxRecordSize || json.Unmarshal(data, v) != nil || v.Validate() != nil {
		return false, nil
	}
	return true, nil
}

// writeRecord writes v, encoded, at path, replacing any file there, so that the last write to complete wins.
func (d *Directory) writeRecord(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := d.create()
	if err != nil {
		return err
	}
	defer f.discard()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.commit(path)
}

// Result returns the latest result for key, or nil if there is none that can be read.
func (d *Directory) Result(_ context.Context, key digest.Digest) (*ActionResult, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	var r ActionResult
	ok, err := readRecord(d.actionPath(key), &r)
	if !ok || err != nil {
		return nil, err
	}
	return &r, nil
}

// SetResult records r as the latest result for key, replacing any earlier one.
func (d *Directory) SetResult(_ context.Context, key digest.Digest, r *ActionResult) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	return d.writeRecord(d.actionPath(key), r)
}

// Tag returns the record of the tag name, or nil if there is none that can be read.
func (d *Directory) Tag(_ context.Context, name string) (*TagRecord, error) {
	var r TagRecord
	ok, err := readRecord(d.tagPath(name), &r)
	if !ok || err != nil {
		return nil, err
	}
	return &r, nil
}

// SetTag records r for the tag name, replacing any earlier record.
func (d *Directory) SetTag(_ context.Context, name string, r *TagRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return d.writeRecord(d.tagPath(name), r)
}

// ClearResults removes every action result, but no tag record, returning how many there were, and advances the
// generation, which the file "generation" holds.
func (d *Directory) ClearResults(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(filepath.Join(d.dir, "ac"))
	if err != nil {
		return 0, err
	}
	err = os.RemoveAll(filepath.Join(d.dir, "ac"))
	if err = errors.Join(err, makeSubdirectories(d.dir)); err != nil {
		return len(entries), err
	}
	generation, err := d.Generation(ctx)
	if err != nil {
		return len(entries), err
	}
	return len(entries), d.writeRecord(d.generationPath(), generation+1)
}

// generationPath is where the generation is kept.
func (d *Directory) generationPath() string {
	return filepath.Join(d.dir, "generation")
}

// Generation returns how many times the results were cleared; a directory never cleared is at 0.
func (d *Directory) Generation(context.Context) (uint64, error) {
	data, err := os.ReadFile(d.generationPath())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("reading the build cache's generation: %w", err)
	}
	var generation uint64
	if err := json.Unmarshal(data, &generation); err != nil {
		return 0, fmt.Errorf("reading the build cache's generation: %w", err)
	}
	return generation, nil
}
