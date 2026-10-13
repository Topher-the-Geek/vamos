// Package cache is the build cache: a content-addressed store of files, trees and logs, and an action cache from
// each target's key to its latest result. It is what the client and an executor share, so a remote cache server
// could stand in for it.
package cache

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// Cache is the local build cache. It has the two parts a remote cache server would hold, so one
// server could later stand in for it: a content-addressed store of blobs, such as files, trees
// of files and logs, and an action cache, which maps each target's key to its latest result.
// The images of layers are kept in the container store instead, and results name them by digest.
type Cache struct {
	dir string // holds cas/sha256/<hex>, ac/<hex>, tags/<hex> and tmp/
}

// Open opens the cache in dir, creating it if need be.
func Open(dir string) (*Cache, error) {
	for _, sub := range []string{"cas/sha256", "ac", "tags", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("opening build cache: %w", err)
		}
	}
	return &Cache{dir: dir}, nil
}

// Blob identifies content in the cache by its digest and size, as a remote cache would.
type Blob struct {
	Digest digest.Digest `json:"digest"`
	Size   int64         `json:"size"`
}

// blobPath returns where the blob with digest d is kept.
func (c *Cache) blobPath(d digest.Digest) string {
	return filepath.Join(c.dir, "cas", d.Algorithm().String(), d.Encoded())
}

// ErrOverLimit means that what was to be stored holds more bytes than allowed.
var ErrOverLimit = errors.New("more bytes than allowed")

// put stores what r holds as a blob, or fails with ErrOverLimit, storing nothing, if it holds more than limit
// bytes. It reads at most one byte past the limit.
func (c *Cache) put(r io.Reader, limit int64) (Blob, error) {
	f, err := c.create()
	if err != nil {
		return Blob{}, err
	}
	defer f.discard()
	if _, err := io.Copy(f, io.LimitReader(r, limit+1)); err != nil {
		return Blob{}, err
	}
	b := f.blob()
	if b.Size > limit {
		return Blob{}, ErrOverLimit
	}
	// The same digest means the same content, so replacing an existing blob changes nothing.
	return b, f.commit(c.blobPath(b.Digest))
}

// pendingFile is a file being written into the cache. It is written to a temporary file and
// renamed into place once complete, so that it is never seen partly written, even by another
// vamos process. Its digest and size are worked out as it is written.
type pendingFile struct {
	f        *os.File
	digester digest.Digester
	size     int64
}

// create starts writing a file into the cache. The caller commits or discards it.
func (c *Cache) create() (*pendingFile, error) {
	f, err := os.CreateTemp(filepath.Join(c.dir, "tmp"), "pending-")
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

// PutBytes stores data as a blob.
func (c *Cache) PutBytes(data []byte) (Blob, error) {
	return c.put(bytes.NewReader(data), int64(len(data)))
}

// OpenBlob opens blob b for reading. An error wrapping fs.ErrNotExist means the cache lacks it.
func (c *Cache) OpenBlob(b Blob) (*os.File, error) {
	return os.Open(c.blobPath(b.Digest))
}

// Has reports why blob b is missing from the cache, if it is.
func (c *Cache) Has(b Blob) error {
	_, err := os.Stat(c.blobPath(b.Digest))
	return err
}

// ReadBytes returns the content of blob b.
func (c *Cache) ReadBytes(b Blob) ([]byte, error) {
	return os.ReadFile(c.blobPath(b.Digest))
}

// Tree is a set of files, directories and symbolic links, kept in the cache as a blob that
// names the blobs of its files. Modification times are left out, so that the same files give
// the same tree wherever and whenever they are made.
type Tree struct {
	Entries []TreeEntry `json:"entries"` // sorted by path, so a directory comes before what it holds
}

// TreeEntry is a file, directory or symbolic link in a tree.
type TreeEntry struct {
	Path   string `json:"path"`             // slash-separated and relative to the tree's root
	Type   string `json:"type"`             // "dir", "file" or "link"
	Mode   uint32 `json:"mode,omitempty"`   // permission and special bits, as chmod takes them; not for links
	Target string `json:"target,omitempty"` // a link's target
	Blob   *Blob  `json:"blob,omitempty"`   // a file's content
}

// The types of tree entries.
const (
	EntryDir  = "dir"
	EntryFile = "file"
	EntryLink = "link"
)

// Encode returns t's canonical encoding, the content of its blob.
func (t *Tree) Encode() []byte {
	data, err := json.Marshal(t)
	if err != nil {
		panic(err) // a tree always encodes
	}
	return data
}

// Digest returns the digest that t's blob has, whether or not it is stored.
func (t *Tree) Digest() Blob {
	data := t.Encode()
	return Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}
}

// Count is how many of each type of entry a tree has, and the size of its files.
type Count struct {
	Files, Dirs, Links int
	Bytes              int64
}

// Args returns c as a trace's arguments: a list of keys and values.
func (c Count) Args() []any {
	return []any{"files", c.Files, "dirs", c.Dirs, "links", c.Links, "bytes", c.Bytes}
}

// Count counts t's entries, and the bytes of its files.
func (t *Tree) Count() Count {
	var c Count
	for _, e := range t.Entries {
		switch e.Type {
		case EntryDir:
			c.Dirs++
		case EntryLink:
			c.Links++
		case EntryFile:
			c.Files++
			if e.Blob != nil {
				c.Bytes += e.Blob.Size
			}
		}
	}
	return c
}

// PutFile stores the file name in fsys as a blob, and reports whether the cache lacked its content. It hashes the
// file first, and copies it only if the cache lacks that content, so a file the cache already holds is read once
// and not written.
// The blob returned is always the content the cache holds, even if the file changes as it is read.
// It fails with ErrOverLimit, storing nothing, if the file holds more than limit bytes, and reads at most
// one byte past the limit, so a file that grows as it is read can't fill the cache.
func (c *Cache) PutFile(fsys afero.Fs, name string, limit int64) (Blob, bool, error) {
	b, err := HashFile(fsys, name, limit)
	if err != nil {
		return Blob{}, false, err
	}
	if c.Has(b) == nil {
		return b, false, nil
	}
	f, err := fsys.Open(name)
	if err != nil {
		return Blob{}, false, err
	}
	defer f.Close()
	b, err = c.put(f, limit)
	return b, err == nil, err
}

// HashFile returns the blob that the file name in fsys would be stored as, storing nothing. It fails with
// ErrOverLimit if the file holds more than limit bytes, and reads at most one byte past the limit.
func HashFile(fsys afero.Fs, name string, limit int64) (Blob, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return Blob{}, err
	}
	defer f.Close()
	digester := digest.Canonical.Digester()
	size, err := io.Copy(digester.Hash(), io.LimitReader(f, limit+1))
	if err != nil {
		return Blob{}, err
	}
	if size > limit {
		return Blob{}, ErrOverLimit
	}
	return Blob{Digest: digester.Digest(), Size: size}, nil
}

// ReadTree returns the tree stored as blob b.
func (c *Cache) ReadTree(b Blob) (*Tree, error) {
	data, err := c.ReadBytes(b)
	if err != nil {
		return nil, err
	}
	var t Tree
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("tree %s: %w", b.Digest, err)
	}
	return &t, nil
}

// CheckTree reports whether the tree stored as blob b, or any of its files, is missing.
func (c *Cache) CheckTree(b Blob) error {
	t, err := c.ReadTree(b)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		if e.Blob != nil {
			if err := c.Has(*e.Blob); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		}
	}
	return nil
}

// Checkout writes the files of the tree stored as blob b into dir on disk, which must exist, and
// returns the tree.
// Folders above an entry that the tree doesn't list are made with the usual mode. Entries that
// would be written outside dir are refused: Walk never makes them, but the cache is a folder
// on disk that anything running as root could change.
func (c *Cache) Checkout(b Blob, dir string) (*Tree, error) {
	t, err := c.ReadTree(b)
	if err != nil {
		return nil, err
	}
	links := map[string]bool{}
	var dirs []TreeEntry
	for _, e := range t.Entries {
		if !filepath.IsLocal(filepath.FromSlash(e.Path)) {
			return nil, fmt.Errorf("tree %s: %q is outside it", b.Digest, e.Path)
		}
		// Links are tracked by path, so a path such as a/../out would hide that out is a link.
		if path.Clean(e.Path) != e.Path {
			return nil, fmt.Errorf("tree %s: %q isn't a clean path", b.Digest, e.Path)
		}
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if links[parent] {
				return nil, fmt.Errorf("tree %s: %q is beneath the symbolic link %q", b.Digest, e.Path, parent)
			}
		}
		dst := filepath.Join(dir, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		switch {
		case e.Type == EntryDir:
			if err := os.Mkdir(dst, 0o700); err != nil {
				return nil, err
			}
			dirs = append(dirs, e)
		case e.Type == EntryLink:
			if err := os.Symlink(e.Target, dst); err != nil {
				return nil, err
			}
			links[e.Path] = true
		case e.Type == EntryFile && e.Blob != nil:
			if err := c.checkoutFile(*e.Blob, dst, e.Mode); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Path, err)
			}
		default:
			return nil, fmt.Errorf("tree %s: %q has unknown type %q or no content", b.Digest, e.Path, e.Type)
		}
	}
	// Set directories' modes once they hold everything, deepest first, in case one is read-only.
	for _, e := range slices.Backward(dirs) {
		if err := os.Chmod(filepath.Join(dir, filepath.FromSlash(e.Path)), fileModeOf(e.Mode)); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// checkoutFile copies blob b to a new file at dst with the given chmod bits.
func (c *Cache) checkoutFile(b Blob, dst string, mode uint32) error {
	in, err := c.OpenBlob(b)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Set the mode afterwards, since the umask would mask it and OpenFile ignores special bits.
	return os.Chmod(dst, fileModeOf(mode))
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

// ActionResult is the latest result of a target. Everything it names is in the cache or, for
// a layer's image, in the container store, so a result can be checked before it is reused.
type ActionResult struct {
	Image  digest.Digest `json:"image,omitempty"`  // a layer's image: its config's digest and store ID
	Output *Blob         `json:"output,omitempty"` // a task's files, as a tree
	Log    *Blob         `json:"log,omitempty"`    // what its run() steps printed, if anything
	Epoch  int64         `json:"epoch"`            // the time its files were stamped with, in Unix seconds
}

// actionPath returns where the result for key is kept.
func (c *Cache) actionPath(key digest.Digest) string {
	return filepath.Join(c.dir, "ac", key.Encoded())
}

// Result returns the latest result for key, or nil if there is none.
func (c *Cache) Result(key digest.Digest) (*ActionResult, error) {
	data, err := os.ReadFile(c.actionPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var r ActionResult
	if err := json.Unmarshal(data, &r); err != nil {
		// A result that can't be read is as good as none; the target's next run replaces it.
		return nil, nil
	}
	return &r, nil
}

// SetResult records r as the latest result for key, replacing any earlier one, so that the last
// of two builds to finish wins.
func (c *Cache) SetResult(key digest.Digest, r *ActionResult) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := c.create()
	if err != nil {
		return err
	}
	defer f.discard()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.commit(c.actionPath(key))
}

// tagPath returns where the record of the tag name is kept.
func (c *Cache) tagPath(name string) string {
	return filepath.Join(c.dir, "tags", digest.FromString(name).Encoded())
}

// Tag returns the record of the tag name, such as an image's tag, or nil if there is none. Records are kept until
// replaced, even by Clear.
func (c *Cache) Tag(name string) ([]byte, error) {
	data, err := os.ReadFile(c.tagPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// SetTag records data for the tag name, replacing any earlier record.
func (c *Cache) SetTag(name string, data []byte) error {
	f, err := c.create()
	if err != nil {
		return err
	}
	defer f.discard()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.commit(c.tagPath(name))
}

// Clear removes every result and blob from the cache, but not tag records, returning how many results and blobs
// it held.
func (c *Cache) Clear() (results, blobs int, err error) {
	resultEntries, _ := os.ReadDir(filepath.Join(c.dir, "ac"))
	blobEntries, _ := os.ReadDir(filepath.Join(c.dir, "cas", "sha256"))
	var errs []error
	for _, sub := range []string{"ac", "cas", "tmp"} {
		if err := os.RemoveAll(filepath.Join(c.dir, sub)); err != nil {
			errs = append(errs, err)
		}
	}
	return len(resultEntries), len(blobEntries), errors.Join(errs...)
}

// The streams a log records.
const (
	StreamStdout byte = 1
	StreamStderr byte = 2
)

// LogRecorder keeps what a target prints to stdout and stderr, in the order it was written,
// so that a later build can replay it when it reuses the target's result. It writes the log to the
// cache as it comes, so that a long one isn't held in memory.
type LogRecorder struct {
	mu   sync.Mutex
	file *pendingFile // records of a stream byte, a uvarint length and that many bytes
	err  error        // the first error writing file
}

// NewLog starts recording a log. The caller saves or discards it.
func (c *Cache) NewLog() (*LogRecorder, error) {
	f, err := c.create()
	if err != nil {
		return nil, err
	}
	return &LogRecorder{file: f}, nil
}

// Writer returns a writer that passes output for stream on to w, recording it as well.
func (l *LogRecorder) Writer(stream byte, w io.Writer) io.Writer {
	return logWriter{l, stream, w}
}

// logWriter records what is written to one stream of a LogRecorder.
type logWriter struct {
	l      *LogRecorder
	stream byte
	w      io.Writer
}

// Write records data and passes it on. An error recording it is kept for save to report, so that
// the target's output still reaches the user.
func (lw logWriter) Write(data []byte) (int, error) {
	lw.l.mu.Lock()
	if lw.l.err == nil {
		header := binary.AppendUvarint([]byte{lw.stream}, uint64(len(data)))
		if _, err := lw.l.file.Write(header); err != nil {
			lw.l.err = err
		} else if _, err := lw.l.file.Write(data); err != nil {
			lw.l.err = err
		}
	}
	lw.l.mu.Unlock()
	return lw.w.Write(data)
}

// Save stores the log as a blob in c, returning nil if nothing was written.
func (l *LogRecorder) Save(c *Cache) (*Blob, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	if l.file.size == 0 {
		return nil, nil
	}
	b := l.file.blob()
	if err := l.file.commit(c.blobPath(b.Digest)); err != nil {
		return nil, err
	}
	return &b, nil
}

// Discard removes the log, unless it has been saved.
func (l *LogRecorder) Discard() {
	l.file.discard()
}

// ReplayLog writes a log recorded by a LogRecorder to stdout and stderr, in its original order.
func ReplayLog(log io.Reader, stdout, stderr io.Writer) error {
	r := bufio.NewReader(log)
	for {
		stream, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return fmt.Errorf("reading log: %w", err)
		}
		w := stdout
		if stream == StreamStderr {
			w = stderr
		}
		if _, err := io.CopyN(w, r, int64(n)); err != nil {
			return fmt.Errorf("reading log: %w", err)
		}
	}
}

// fileMode returns the permission and special bits of info's mode.
func fileMode(info fs.FileInfo) fs.FileMode {
	return info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}
