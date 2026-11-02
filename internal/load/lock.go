package load

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
	"go.podman.io/image/v5/docker/reference"
	"golang.org/x/sys/unix"
)

// LockFile is the name of the lockfile, which pins pulls, at the workspace's root.
const LockFile = "vamos.lock"

// LockVersion is the version of the lockfile that this vamos reads and writes.
// A reader ignores fields it doesn't know, so a field can be added within a version;
// a lockfile of a greater version is refused, so that an older vamos never rewrites it and drops what it can't read.
const LockVersion = 1

// Lock is a lockfile: the pin of each pulled image and tag.
// It keeps the fields that this vamos doesn't know, as a newer one of the same version may write,
// and writes them back: the lockfile's, and a pin's while the pin is unchanged.
type Lock struct {
	Version int
	// Pulls are the pins by image and tag, normalized, as graph.Pull.Image gives them:
	// "docker.io/library/node:24-alpine".
	Pulls map[string]Pin

	unknown     map[string]json.RawMessage // the lockfile's fields that this vamos doesn't know
	unknownPins map[string]unknownPin      // those of each pin, by image, while it is the pin read
}

// unknownPin is the fields of a pin that this vamos doesn't know, and the pin they were read with.
type unknownPin struct {
	read   Pin
	fields map[string]json.RawMessage
}

// Pin is what a pulled tag resolved to.
type Pin struct {
	Tag    string        `json:"tag"`    // the tag resolved to, which may be the tag pulled or one it covers
	Digest digest.Digest `json:"digest"` // of the manifest the tag resolved to named: an index or an image
}

// NewLock returns a lockfile that pins nothing.
func NewLock() *Lock {
	return &Lock{Version: LockVersion, Pulls: map[string]Pin{}}
}

// LockPath returns the path of the lockfile of the workspace whose root is root.
func LockPath(root string) string {
	return filepath.Join(root, LockFile)
}

// ReadLock reads the lockfile of the workspace whose root is root, in fsys, or returns one that pins nothing if there
// is none. A malformed lockfile, or one of a newer version, is refused, naming it,
// and a syntax error's line and column.
func ReadLock(fsys afero.Fs, root string) (*Lock, error) {
	path := LockPath(root)
	data, err := afero.ReadFile(fsys, path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewLock(), nil
	} else if err != nil {
		return nil, err
	}
	return parseLock(path, data)
}

// parseLock parses data, the lockfile at path, and checks it.
// Its version is read first, so that a newer lockfile is refused as one, whatever else it holds.
// What it holds is read from its fields by their exact names, as the last of any repeated field gives them,
// once decoding the whole of data has checked their kinds, saying where a mistake is;
// a field that differs from a known field's name only in case is refused, since that decoding takes it for that field.
func parseLock(path string, data []byte) (*Lock, error) {
	var fields map[string]json.RawMessage
	if err := unmarshal(path, data, &fields); err != nil {
		return nil, err
	}
	l := &Lock{Pulls: map[string]Pin{}}
	if err := unmarshal(path, data, &struct {
		Version *int `json:"version"`
	}{new(int)}); err != nil {
		return nil, err
	}
	version, given := fields["version"]
	if given && string(version) != "null" {
		if err := json.Unmarshal(version, &l.Version); err != nil {
			return nil, fmt.Errorf("%s: \"version\" holds %s, want a number", path, version)
		}
	}
	switch {
	case l.Version > LockVersion:
		return nil, fmt.Errorf("%s is of version %d, but this vamos reads only version %d or older; upgrade vamos",
			path, l.Version, LockVersion)
	case !given || string(version) == "null":
		return nil, fmt.Errorf("%s has no version", path)
	case l.Version < 1:
		return nil, fmt.Errorf("%s is of version %d, which no vamos writes", path, l.Version)
	}
	if err := checkCase(fields, "version", "pulls"); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := unmarshal(path, data, &struct {
		Pulls map[string]Pin `json:"pulls"`
	}{}); err != nil {
		return nil, err
	}
	var pulls map[string]map[string]json.RawMessage
	if err := json.Unmarshal(fields["pulls"], &pulls); fields["pulls"] != nil && err != nil {
		return nil, fmt.Errorf("%s: \"pulls\": %w", path, err)
	}
	for _, image := range slices.Sorted(maps.Keys(pulls)) {
		pin, err := readPin(image, pulls[image])
		if err != nil {
			return nil, fmt.Errorf("%s: pull %q: %w", path, image, err)
		}
		l.Pulls[image] = pin
		if extra := withoutKnown(pulls[image], "tag", "digest"); extra != nil {
			if l.unknownPins == nil {
				l.unknownPins = map[string]unknownPin{}
			}
			l.unknownPins[image] = unknownPin{read: pin, fields: extra}
		}
	}
	l.unknown = withoutKnown(fields, "version", "pulls")
	return l, nil
}

// readPin returns the pin of image that fields give, by their exact names, and checks it.
func readPin(image string, fields map[string]json.RawMessage) (Pin, error) {
	if err := checkCase(fields, "tag", "digest"); err != nil {
		return Pin{}, err
	}
	var pin Pin
	for name, v := range map[string]any{"tag": &pin.Tag, "digest": &pin.Digest} {
		if raw, ok := fields[name]; ok {
			if err := json.Unmarshal(raw, v); err != nil {
				return Pin{}, fmt.Errorf("%q holds %s, want a string", name, raw)
			}
		}
	}
	return pin, checkPin(image, pin)
}

// checkCase refuses a field of fields whose name differs from one of known only in case.
func checkCase(fields map[string]json.RawMessage, known ...string) error {
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		for _, k := range known {
			if name != k && strings.EqualFold(name, k) {
				return fmt.Errorf("field %q is written %q", name, k)
			}
		}
	}
	return nil
}

// unmarshal decodes data, the lockfile at path, into v, saying where in it a syntax or type error is,
// and a type error in a lockfile's terms rather than Go's.
func unmarshal(path string, data []byte, v any) error {
	err := json.Unmarshal(data, v)
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &syntax):
		line, col := lineCol(data, syntax.Offset)
		return fmt.Errorf("%s:%d:%d: %v", path, line, col, err)
	case errors.As(err, &typ):
		line, col := lineCol(data, typ.Offset)
		where := "the lockfile"
		if f := strings.TrimPrefix(typ.Field, "."); f != "" {
			where = strconv.Quote(f)
		}
		return fmt.Errorf("%s:%d:%d: %s holds %s, want %s", path, line, col, where, withArticle(typ.Value),
			withArticle(jsonKind(typ.Type)))
	}
	return fmt.Errorf("%s: %w", path, err)
}

// jsonKind returns what JSON calls a value that decodes into t.
func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return "object"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	default:
		return "number"
	}
}

// withArticle returns kind, a kind of JSON value as encoding/json names it, with its article: "an object".
func withArticle(kind string) string {
	if strings.ContainsRune("aeiou", rune(kind[0])) {
		return "an " + kind
	}
	return "a " + kind
}

// withoutKnown returns fields without those named known, or nil if there are no others.
func withoutKnown(fields map[string]json.RawMessage, known ...string) map[string]json.RawMessage {
	var extra map[string]json.RawMessage
	for name, value := range fields {
		if !slices.Contains(known, name) {
			if extra == nil {
				extra = map[string]json.RawMessage{}
			}
			extra[name] = value
		}
	}
	return extra
}

// checkPin checks that image is a normalized name and tag, as graph.Pull.Image gives one, and that pin pins it.
func checkPin(image string, pin Pin) error {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return err
	}
	if _, ok := named.(reference.NamedTagged); !ok {
		return errors.New("it has no tag")
	}
	if _, ok := named.(reference.Digested); ok {
		return errors.New("it has a digest")
	}
	if named.String() != image {
		return fmt.Errorf("it is written %q", named.String())
	}
	if pin.Tag == "" {
		return errors.New("it has no tag resolved to")
	}
	if _, err := reference.WithTag(reference.TrimNamed(named), pin.Tag); err != nil {
		return fmt.Errorf("tag resolved to %q: %w", pin.Tag, err)
	}
	if err := pin.Digest.Validate(); err != nil {
		return fmt.Errorf("digest %q: %w", pin.Digest, err)
	}
	if pin.Digest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("digest %q: want a sha256 digest, which pulling verifies the manifest by", pin.Digest)
	}
	return nil
}

// lineCol returns the line and column, from 1, of the byte before offset in data, as encoding/json counts offsets:
// a syntax error's is the byte it found wrong, and a type error's the last of the value it couldn't decode.
func lineCol(data []byte, offset int64) (line, col int) {
	offset = max(0, min(offset, int64(len(data)))-1)
	before := data[:offset]
	line = bytes.Count(before, []byte("\n")) + 1
	col = int(offset) - (bytes.LastIndexByte(before, '\n') + 1) + 1
	return line, col
}

// Marshal returns the lockfile as vamos writes it, one field per line, indented by two spaces, and ending in a
// newline: its version, then its pins, sorted by image, each its tag then its digest, and, after the fields of each,
// those this vamos doesn't know, sorted. The same pins always give the same bytes, and one pin's change changes only
// its lines.
func (l *Lock) Marshal() []byte {
	pulls := make([]field, 0, len(l.Pulls))
	for _, image := range slices.Sorted(maps.Keys(l.Pulls)) {
		pin := l.Pulls[image]
		var extra map[string]json.RawMessage
		if u, ok := l.unknownPins[image]; ok && u.read == pin {
			extra = u.fields
		}
		pulls = append(pulls, field{image, object(extra, field{"tag", pin.Tag}, field{"digest", pin.Digest})})
	}
	compact := object(l.unknown, field{"version", l.Version}, field{"pulls", object(nil, pulls...)})
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		panic(err) // object writes valid JSON
	}
	out.WriteByte('\n')
	return out.Bytes()
}

// field is a field of a JSON object: its name and value, which object marshals unless it is JSON already.
type field struct {
	name  string
	value any
}

// object returns the compact JSON of an object of fields, in order, then of extra's fields, sorted by name.
func object(extra map[string]json.RawMessage, fields ...field) json.RawMessage {
	for _, name := range slices.Sorted(maps.Keys(extra)) {
		fields = append(fields, field{name, extra[name]})
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(marshal(f.name))
		b.WriteByte(':')
		b.Write(marshal(f.value))
	}
	b.WriteByte('}')
	return b.Bytes()
}

// marshal returns the JSON of v, which can't fail for the strings, numbers and JSON that a lockfile holds.
func marshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// UpdateLock changes the lockfile of the workspace whose root is root, in fsys: holding the workspace's lock, which
// another UpdateLock takes, it reads the lockfile again, as it is now, applies change, and writes it whole, of this
// vamos's version, to a temporary file synced and renamed over it, so that a reader never sees it half written,
// a crash leaves the old lockfile or the new one, and a pin written since the caller last read it is kept.
// It writes nothing if change leaves the lockfile's bytes as they were, or if there was none and change pins nothing,
// and reports whether it wrote. Waiting for the lock ends with ctx.
func UpdateLock(ctx context.Context, fsys afero.Fs, root string, change func(*Lock)) (wrote bool, err error) {
	path := LockPath(root)
	unlock, err := lockWorkspace(ctx, fsys, root)
	if err != nil {
		return false, fmt.Errorf("locking %s: %w", path, err)
	}
	defer unlock()
	after, changed, err := changeLock(fsys, path, change)
	if err != nil || !changed {
		return false, err
	}
	return true, writeFile(fsys, path, after)
}

// WouldUpdateLock reports whether UpdateLock would write the lockfile of the workspace whose root is root, in fsys,
// with change, as it is now, writing nothing. It takes no lock: a writer renames a whole lockfile into place, so it
// reads one lockfile or the other, never part of one.
func WouldUpdateLock(fsys afero.Fs, root string, change func(*Lock)) (bool, error) {
	_, changed, err := changeLock(fsys, LockPath(root), change)
	return changed, err
}

// changeLock reads the lockfile at path, in fsys, applies change, and returns the lockfile as UpdateLock would write
// it, of this vamos's version, and whether that differs from the one there: in its bytes, or by pinning something
// where there is none.
func changeLock(fsys afero.Fs, path string, change func(*Lock)) ([]byte, bool, error) {
	before, err := afero.ReadFile(fsys, path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	l := NewLock()
	if exists {
		if l, err = parseLock(path, before); err != nil {
			return nil, false, err
		}
	}
	change(l)
	l.Version = LockVersion
	for _, image := range slices.Sorted(maps.Keys(l.Pulls)) {
		if err := checkPin(image, l.Pulls[image]); err != nil {
			return nil, false, fmt.Errorf("writing %s: pull %q: %w", path, image, err) // a bug in what pins it
		}
	}
	after := l.Marshal()
	if exists && bytes.Equal(before, after) || !exists && len(l.Pulls) == 0 {
		return after, false, nil
	}
	return after, true, nil
}

// processLock serializes UpdateLock within this process on a filesystem whose directories can't be locked,
// such as one in memory: it holds a value while UpdateLock holds the lock.
var processLock = make(chan struct{}, 1)

// lockPoll is how often lockWorkspace tries again for a lock that another holds.
const lockPoll = 20 * time.Millisecond

// lockWorkspace takes the lock on the workspace whose root is root, in fsys, for writing its lockfile, waiting for it
// until ctx ends, and returns what releases it. On disk this is an flock of the root directory, which other processes
// take too, and which leaves no file of its own behind.
func lockWorkspace(ctx context.Context, fsys afero.Fs, root string) (unlock func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := fsys.Open(root)
	if err != nil {
		return nil, err
	}
	f := afero.File(dir)
	if base, ok := f.(*afero.BasePathFile); ok {
		f = base.File
	}
	osFile, ok := f.(*os.File)
	if !ok {
		_ = dir.Close()
		select {
		case processLock <- struct{}{}:
			return func() { <-processLock }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	for {
		err = unix.Flock(int(osFile.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			break
		}
		select {
		case <-time.After(lockPoll):
		case <-ctx.Done():
			_ = dir.Close()
			return nil, ctx.Err()
		}
	}
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	return func() { _ = dir.Close() }, nil // closing releases the flock
}

// writeFile writes data to path in fsys durably: to a temporary file beside it, synced, renamed over it, and its
// directory synced.
func writeFile(fsys afero.Fs, path string, data []byte) (err error) {
	f, err := afero.TempFile(fsys, filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = fsys.Remove(f.Name())
			err = fmt.Errorf("writing %s: %w", path, err)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := fsys.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	if err := fsys.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := fsys.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
