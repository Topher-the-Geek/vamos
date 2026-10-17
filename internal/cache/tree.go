package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
)

// Tree is a set of files, directories and symbolic links,
// kept in the content store as a blob that names the blobs of its files.
// Modification times are left out, so that the same files give the same tree wherever and whenever they are made.
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

// Validate checks that t is a tree that Checkout can write safely, and that fileset.Walk could have made:
// its paths are unique, sorted, clean and within the tree, no entry lies beneath a symbolic link or a file,
// and each entry has the fields of its type, and no others.
func (t *Tree) Validate() error {
	links := map[string]bool{}
	files := map[string]bool{}
	for i, e := range t.Entries {
		p := e.Path
		switch {
		case p == "" || p == ".":
			return fmt.Errorf("entry %d has no path", i)
		case path.Clean(p) != p || strings.HasPrefix(p, "/") || !filepath.IsLocal(filepath.FromSlash(p)):
			return fmt.Errorf("%q isn't a clean path within the tree", p)
		case strings.ContainsRune(p, 0):
			return fmt.Errorf("%q holds a NUL byte", p)
		case i > 0 && t.Entries[i-1].Path >= p:
			return fmt.Errorf("%q isn't after %q, so the entries aren't sorted and unique", p, t.Entries[i-1].Path)
		case e.Mode > 0o7777:
			return fmt.Errorf("%q has mode %o, which isn't chmod bits", p, e.Mode)
		}
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if links[parent] {
				return fmt.Errorf("%q is beneath the symbolic link %q", p, parent)
			}
			if files[parent] {
				return fmt.Errorf("%q is beneath the file %q", p, parent)
			}
		}
		switch e.Type {
		case EntryDir:
			if e.Target != "" || e.Blob != nil {
				return fmt.Errorf("directory %q has a link target or content", p)
			}
		case EntryFile:
			if e.Blob == nil {
				return fmt.Errorf("file %q has no content", p)
			}
			if err := e.Blob.Validate(); err != nil {
				return fmt.Errorf("file %q: %w", p, err)
			}
			if e.Target != "" {
				return fmt.Errorf("file %q has a link target", p)
			}
			files[p] = true
		case EntryLink:
			if e.Target == "" || strings.ContainsRune(e.Target, 0) {
				return fmt.Errorf("link %q has no target, or one with a NUL byte", p)
			}
			if e.Mode != 0 || e.Blob != nil {
				return fmt.Errorf("link %q has a mode or content", p)
			}
			links[p] = true
		default:
			return fmt.Errorf("%q has unknown type %q", p, e.Type)
		}
	}
	return nil
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

// Files returns the blobs of t's files.
func (t *Tree) Files() []Blob {
	var blobs []Blob
	for _, e := range t.Entries {
		if e.Type == EntryFile && e.Blob != nil {
			blobs = append(blobs, *e.Blob)
		}
	}
	return blobs
}

// ReadTree returns the tree stored as blob b in store, checked.
// A blob that isn't a valid tree is reported as corrupt.
func ReadTree(ctx context.Context, store ContentStore, b Blob) (*Tree, error) {
	data, err := store.Read(ctx, b, MaxTreeSize)
	if err != nil {
		return nil, fmt.Errorf("tree %s: %w", b.Digest, err)
	}
	var t Tree
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, Corrupt(b, "it isn't a tree: "+err.Error())
	}
	if err := t.Validate(); err != nil {
		return nil, Corrupt(b, "it isn't a valid tree: "+err.Error())
	}
	return &t, nil
}

// CheckTree reports whether the tree stored as blob b in store, or any of its files, is missing or corrupt,
// as far as a store can tell without reading the files: Get verifies each file's content when it is read.
func CheckTree(ctx context.Context, store ContentStore, b Blob) error {
	t, err := ReadTree(ctx, store, b)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		if e.Blob != nil {
			if err := store.Has(ctx, *e.Blob); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		}
	}
	return nil
}

// Checkout writes the files of the tree stored as blob b in store into dir on disk, which must exist,
// and returns the tree.
// Folders above an entry that the tree doesn't list are made with the usual mode.
// The tree is validated first, so entries that would be written outside dir are refused:
// Walk never makes them, but a store on disk is a folder that anything running as root could change.
// A file whose content doesn't match its blob fails the checkout with ErrCorrupt.
// The checkout stops with ctx's error, between entries, once ctx ends.
func Checkout(ctx context.Context, store ContentStore, b Blob, dir string) (*Tree, error) {
	t, err := ReadTree(ctx, store, b)
	if err != nil {
		return nil, err
	}
	var dirs []TreeEntry
	for _, e := range t.Entries {
		// A checkout of many files stops between them if ctx ends.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dst := filepath.Join(dir, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		switch e.Type {
		case EntryDir:
			if err := os.Mkdir(dst, 0o700); err != nil {
				return nil, err
			}
			dirs = append(dirs, e)
		case EntryLink:
			if err := os.Symlink(e.Target, dst); err != nil {
				return nil, err
			}
		case EntryFile:
			if err := checkoutFile(ctx, store, *e.Blob, dst, e.Mode); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Path, err)
			}
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

// checkoutFile copies blob b from store to a new file at dst with the given chmod bits.
func checkoutFile(ctx context.Context, store ContentStore, b Blob, dst string, mode uint32) error {
	in, err := store.Get(ctx, b)
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
