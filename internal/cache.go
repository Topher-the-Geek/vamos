package internal

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
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// cache is the local build cache. It has the two parts a remote cache server would hold, so one
// server could later stand in for it: a content-addressed store of blobs, such as files, trees
// of files and logs, and an action cache, which maps each target's key to its latest result.
// The images of layers are kept in the container store instead, and results name them by digest.
type cache struct {
	dir string // holds cas/sha256/<hex>, ac/<hex> and tmp/
}

// openCache opens the cache in dir, creating it if need be.
func openCache(dir string) (*cache, error) {
	for _, sub := range []string{"cas/sha256", "ac", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("opening build cache: %w", err)
		}
	}
	return &cache{dir: dir}, nil
}

// blob identifies content in the cache by its digest and size, as a remote cache would.
type blob struct {
	Digest digest.Digest `json:"digest"`
	Size   int64         `json:"size"`
}

// blobPath returns where the blob with digest d is kept.
func (c *cache) blobPath(d digest.Digest) string {
	return filepath.Join(c.dir, "cas", d.Algorithm().String(), d.Encoded())
}

// put stores what r holds as a blob.
func (c *cache) put(r io.Reader) (blob, error) {
	f, err := c.create()
	if err != nil {
		return blob{}, err
	}
	defer f.discard()
	if _, err := io.Copy(f, r); err != nil {
		return blob{}, err
	}
	b := f.blob()
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
func (c *cache) create() (*pendingFile, error) {
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
func (p *pendingFile) blob() blob {
	return blob{Digest: p.digester.Digest(), Size: p.size}
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

// putBytes stores data as a blob.
func (c *cache) putBytes(data []byte) (blob, error) {
	return c.put(bytes.NewReader(data))
}

// open opens blob b for reading. An error wrapping fs.ErrNotExist means the cache lacks it.
func (c *cache) open(b blob) (*os.File, error) {
	return os.Open(c.blobPath(b.Digest))
}

// readBytes returns the content of blob b.
func (c *cache) readBytes(b blob) ([]byte, error) {
	return os.ReadFile(c.blobPath(b.Digest))
}

// tree is a set of files, directories and symbolic links, kept in the cache as a blob that
// names the blobs of its files. Modification times are left out, so that the same files give
// the same tree wherever and whenever they are made.
type tree struct {
	Entries []treeEntry `json:"entries"` // sorted by path, so a directory comes before what it holds
}

// treeEntry is a file, directory or symbolic link in a tree.
type treeEntry struct {
	Path   string `json:"path"`             // slash-separated and relative to the tree's root
	Type   string `json:"type"`             // "dir", "file" or "link"
	Mode   uint32 `json:"mode,omitempty"`   // permission and special bits, as chmod takes them; not for links
	Target string `json:"target,omitempty"` // a link's target
	Blob   *blob  `json:"blob,omitempty"`   // a file's content
}

// The types of tree entries.
const (
	entryDir  = "dir"
	entryFile = "file"
	entryLink = "link"
)

// encode returns t's canonical encoding, the content of its blob.
func (t *tree) encode() []byte {
	data, err := json.Marshal(t)
	if err != nil {
		panic(err) // a tree always encodes
	}
	return data
}

// digest returns the digest that t's blob has, whether or not it is stored.
func (t *tree) digest() blob {
	data := t.encode()
	return blob{Digest: digest.FromBytes(data), Size: int64(len(data))}
}

// putFile stores the file name in fsys as a blob. It hashes the file first, and copies it only if
// the cache lacks that content, so a file the cache already holds is read once and not written.
// The blob returned is always the content the cache holds, even if the file changes as it is read.
func (c *cache) putFile(fsys afero.Fs, name string) (blob, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return blob{}, err
	}
	digester := digest.Canonical.Digester()
	size, err := io.Copy(digester.Hash(), f)
	f.Close()
	if err != nil {
		return blob{}, err
	}
	b := blob{Digest: digester.Digest(), Size: size}
	if _, err := os.Stat(c.blobPath(b.Digest)); err == nil {
		return b, nil
	}
	f, err = fsys.Open(name)
	if err != nil {
		return blob{}, err
	}
	defer f.Close()
	return c.put(f)
}

// putFiles stores the files at paths within dir in fsys, and returns the tree that describes
// them, without storing it. Paths are cleaned and relative, as sourcePaths returns them.
func (c *cache) putFiles(fsys afero.Fs, dir string, paths []string) (*tree, error) {
	return walkTree(fsys, dir, paths, func(name string) (blob, error) { return c.putFile(fsys, name) })
}

// putTree stores the files at paths within dir in fsys, and then the tree that describes them.
func (c *cache) putTree(fsys afero.Fs, dir string, paths []string) (blob, error) {
	t, err := c.putFiles(fsys, dir, paths)
	if err != nil {
		return blob{}, err
	}
	return c.putBytes(t.encode())
}

// readTree returns the tree stored as blob b.
func (c *cache) readTree(b blob) (*tree, error) {
	data, err := c.readBytes(b)
	if err != nil {
		return nil, err
	}
	var t tree
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("tree %s: %w", b.Digest, err)
	}
	return &t, nil
}

// checkTree reports whether the tree stored as blob b, or any of its files, is missing.
func (c *cache) checkTree(b blob) error {
	t, err := c.readTree(b)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		if e.Blob != nil {
			if _, err := os.Stat(c.blobPath(e.Blob.Digest)); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		}
	}
	return nil
}

// walkTree describes the files at paths within dir in fsys as a tree, passing each file's
// name to file for its blob. It follows a link at a path itself, as at a project directory
// reached through a linked $PWD, but records links within directories as links, as Docker's
// COPY does, so they can't bring in files from outside the project. checkInclude refuses links
// anywhere else on the way to a path.
func walkTree(fsys afero.Fs, dir string, paths []string, file func(name string) (blob, error)) (*tree, error) {
	t := &tree{}
	var add func(src, rel string, info fs.FileInfo) error
	add = func(src, rel string, info fs.FileInfo) error {
		entry := treeEntry{Path: filepath.ToSlash(rel), Mode: chmodBits(info)}
		switch {
		case info.IsDir():
			entry.Type = entryDir
			names, err := readDirNames(fsys, src)
			if err != nil {
				return err
			}
			for _, name := range names {
				info, _, err := lstat(fsys, filepath.Join(src, name))
				if err != nil {
					return err
				}
				if err := add(filepath.Join(src, name), filepath.Join(rel, name), info); err != nil {
					return err
				}
			}
		case info.Mode()&fs.ModeSymlink != 0:
			reader, ok := fsys.(afero.LinkReader)
			if !ok {
				return fmt.Errorf("%s: cannot read symbolic links", src)
			}
			target, err := reader.ReadlinkIfPossible(src)
			if err != nil {
				return err
			}
			entry.Type, entry.Mode, entry.Target = entryLink, 0, target
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file, directory or symbolic link", src)
		default:
			b, err := file(src)
			if err != nil {
				return err
			}
			entry.Type, entry.Blob = entryFile, &b
		}
		// The root of the tree is where its files go, not one of them, so it has no entry.
		if rel != "." {
			t.Entries = append(t.Entries, entry)
		}
		return nil
	}
	for _, p := range paths {
		src := filepath.Join(dir, p)
		info, err := fsys.Stat(src)
		if err != nil {
			return nil, err
		}
		if err := add(src, p, info); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(t.Entries, func(a, b treeEntry) int { return strings.Compare(a.Path, b.Path) })
	return t, nil
}

// checkout writes the files of the tree stored as blob b into dir on disk, which must exist.
// Folders above an entry that the tree doesn't list are made with the usual mode. Entries that
// would be written outside dir are refused: walkTree never makes them, but the cache is a folder
// on disk that anything running as root could change.
func (c *cache) checkout(b blob, dir string) error {
	t, err := c.readTree(b)
	if err != nil {
		return err
	}
	links := map[string]bool{}
	var dirs []treeEntry
	for _, e := range t.Entries {
		if !filepath.IsLocal(filepath.FromSlash(e.Path)) {
			return fmt.Errorf("tree %s: %q is outside it", b.Digest, e.Path)
		}
		// Links are tracked by path, so a path such as a/../out would hide that out is a link.
		if path.Clean(e.Path) != e.Path {
			return fmt.Errorf("tree %s: %q isn't a clean path", b.Digest, e.Path)
		}
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if links[parent] {
				return fmt.Errorf("tree %s: %q is beneath the symbolic link %q", b.Digest, e.Path, parent)
			}
		}
		dst := filepath.Join(dir, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		switch {
		case e.Type == entryDir:
			if err := os.Mkdir(dst, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, e)
		case e.Type == entryLink:
			if err := os.Symlink(e.Target, dst); err != nil {
				return err
			}
			links[e.Path] = true
		case e.Type == entryFile && e.Blob != nil:
			if err := c.checkoutFile(*e.Blob, dst, e.Mode); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		default:
			return fmt.Errorf("tree %s: %q has unknown type %q or no content", b.Digest, e.Path, e.Type)
		}
	}
	// Set directories' modes once they hold everything, deepest first, in case one is read-only.
	for _, e := range slices.Backward(dirs) {
		if err := os.Chmod(filepath.Join(dir, filepath.FromSlash(e.Path)), fileModeOf(e.Mode)); err != nil {
			return err
		}
	}
	return nil
}

// checkoutFile copies blob b to a new file at dst with the given chmod bits.
func (c *cache) checkoutFile(b blob, dst string, mode uint32) error {
	in, err := c.open(b)
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

// chmodBits returns the permission and special bits of info's mode, as chmod takes them.
func chmodBits(info fs.FileInfo) uint32 {
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

// actionVersion changes whenever what a key covers, or how a target is built, changes, so that
// results built the old way aren't reused.
const actionVersion = 3

// action is everything a target's result depends on. Its digest is the target's key in the
// action cache. Target names are left out, so renaming a target keeps its results.
type action struct {
	Version int          `json:"version"`
	Kind    string       `json:"kind"`              // "layer" or "task"
	Base    string       `json:"base"`              // image ID of the base, or "scratch"
	Steps   []actionStep `json:"steps,omitempty"`   // in the order applied
	Command []string     `json:"command,omitempty"` // a layer's default command
	Source  string       `json:"source,omitempty"`  // a task's directory of output
	Include []string     `json:"include,omitempty"` // a task's output, within source
}

// actionStep is one step of an action. Op says which step, and which other
// fields it uses.
type actionStep struct {
	Op    string   `json:"op"`              // "add", "run", "env", "build_env" or "workdir"
	Files *blob    `json:"files,omitempty"` // add: tree of the files, as placed beneath dest
	Dest  string   `json:"dest,omitempty"`  // add
	Argv  []string `json:"argv,omitempty"`  // run
	Vars  []string `json:"vars,omitempty"`  // env and build_env: NAME=value, sorted
	Dir   string   `json:"dir,omitempty"`   // workdir
}

// key returns a's key in the action cache.
func (a *action) key() digest.Digest {
	data, err := json.Marshal(a)
	if err != nil {
		panic(err) // an action always encodes
	}
	return digest.FromBytes(data)
}

// actionResult is the latest result of a target. Everything it names is in the cache or, for
// a layer's image, in the container store, so a result can be checked before it is reused.
type actionResult struct {
	Image  digest.Digest `json:"image,omitempty"`  // a layer's image: its config's digest and store ID
	Output *blob         `json:"output,omitempty"` // a task's files, as a tree
	Log    *blob         `json:"log,omitempty"`    // what its run() steps printed, if anything
	Epoch  int64         `json:"epoch"`            // the time its files were stamped with, in Unix seconds
}

// actionPath returns where the result for key is kept.
func (c *cache) actionPath(key digest.Digest) string {
	return filepath.Join(c.dir, "ac", key.Encoded())
}

// result returns the latest result for key, or nil if there is none.
func (c *cache) result(key digest.Digest) (*actionResult, error) {
	data, err := os.ReadFile(c.actionPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var r actionResult
	if err := json.Unmarshal(data, &r); err != nil {
		// A result that can't be read is as good as none; the target's next run replaces it.
		return nil, nil
	}
	return &r, nil
}

// setResult records r as the latest result for key, replacing any earlier one, so that the last
// of two builds to finish wins.
func (c *cache) setResult(key digest.Digest, r *actionResult) error {
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

// The streams a log records.
const (
	streamStdout byte = 1
	streamStderr byte = 2
)

// logRecorder keeps what a target prints to stdout and stderr, in the order it was written,
// so that a later build can replay it when it reuses the target's result. It writes the log to the
// cache as it comes, so that a long one isn't held in memory.
type logRecorder struct {
	mu   sync.Mutex
	file *pendingFile // records of a stream byte, a uvarint length and that many bytes
	err  error        // the first error writing file
}

// newLog starts recording a log. The caller saves or discards it.
func (c *cache) newLog() (*logRecorder, error) {
	f, err := c.create()
	if err != nil {
		return nil, err
	}
	return &logRecorder{file: f}, nil
}

// writer returns a writer that passes output for stream on to w, recording it as well.
func (l *logRecorder) writer(stream byte, w io.Writer) io.Writer {
	return logWriter{l, stream, w}
}

// logWriter records what is written to one stream of a logRecorder.
type logWriter struct {
	l      *logRecorder
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

// save stores the log as a blob in c, returning nil if nothing was written.
func (l *logRecorder) save(c *cache) (*blob, error) {
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

// discard removes the log, unless it has been saved.
func (l *logRecorder) discard() {
	l.file.discard()
}

// replayLog writes a log recorded by a logRecorder to stdout and stderr, in its original order.
func replayLog(log io.Reader, stdout, stderr io.Writer) error {
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
		if stream == streamStderr {
			w = stderr
		}
		if _, err := io.CopyN(w, r, int64(n)); err != nil {
			return fmt.Errorf("reading log: %w", err)
		}
	}
}
