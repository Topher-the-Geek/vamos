package cache

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
)

// Blob identifies content by its digest and size, as a remote store would.
type Blob struct {
	Digest digest.Digest `json:"digest"`
	Size   int64         `json:"size"`
}

// Validate checks that b names content as a store expects: a well-formed SHA-256 digest and a size,
// so that a malformed one is refused before it reaches a path or a request.
func (b Blob) Validate() error {
	if err := b.Digest.Validate(); err != nil {
		return fmt.Errorf("blob %q: %w", b.Digest, err)
	}
	if b.Digest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("blob %s: want a sha256 digest", b.Digest)
	}
	if b.Size < 0 {
		return fmt.Errorf("blob %s: negative size %d", b.Digest, b.Size)
	}
	return nil
}

// validateKey checks that key, an action's, is a well-formed SHA-256 digest, as an action's key always is.
func validateKey(key digest.Digest) error {
	return Blob{Digest: key}.Validate()
}

// The errors that stores report, which callers tell apart with errors.Is.
var (
	ErrMissing   = errors.New("missing from the store")
	ErrCorrupt   = errors.New("corrupt")
	ErrOverLimit = errors.New("more bytes than allowed")
)

// CorruptError says that a blob's content doesn't match its digest, or isn't what it should decode to.
// It is ErrCorrupt, for errors.Is, and names the blob, so that a failure can say which one.
type CorruptError struct {
	Blob   Blob
	Reason string
}

// Error describes the corruption.
func (e *CorruptError) Error() string {
	return fmt.Sprintf("blob %s is corrupt: %s", e.Blob.Digest, e.Reason)
}

// Is reports whether target is ErrCorrupt.
func (e *CorruptError) Is(target error) bool {
	return target == ErrCorrupt
}

// Corrupt returns the error for blob b, corrupt as reason says.
func Corrupt(b Blob, reason string) error {
	return &CorruptError{Blob: b, Reason: reason}
}

// missing returns the error for blob b, which the store lacks.
func missing(b Blob) error {
	return fmt.Errorf("blob %s: %w", b.Digest, ErrMissing)
}

// The most bytes that a stored structure may have, so that a corrupt or hostile one is refused rather than decoded.
const (
	MaxTreeSize   = 64 << 20 // a tree of files; 10,000 entries take under 2 MiB
	MaxRecordSize = 1 << 20  // an action result or tag record, which holds a manifest of a few KB
	MaxLogSize    = 64 << 20 // a target's output
)

// ContentStore holds blobs by digest: graphs, source files, trees, logs and, on demand, exported image blobs.
// Every method validates the digests it is given and fails with ErrMissing, ErrCorrupt, ErrOverLimit
// or, when the store can't be reached, another error.
// Operations that touch the local filesystem, such as PutFile and Checkout, are functions over a store,
// so that a remote store needs no more than these methods.
type ContentStore interface {
	// Has returns nil if the store holds b, ErrMissing if it doesn't,
	// and ErrCorrupt if what it holds under b's digest can't be b's content.
	Has(ctx context.Context, b Blob) error
	// Missing returns which of blobs the store lacks, or holds corrupt, so that a caller can put them.
	Missing(ctx context.Context, blobs []Blob) ([]Blob, error)
	// Put stores what r holds, up to limit bytes, and returns its blob: the digest of what was actually read.
	// It fails with ErrOverLimit, storing nothing, if r holds more, reading at most one byte past the limit.
	Put(ctx context.Context, r io.Reader, limit int64) (Blob, error)
	// PutBytes stores data and returns its blob.
	PutBytes(ctx context.Context, data []byte) (Blob, error)
	// Import stores what r holds as b, which must be its digest and size, as advertised by whoever sent it:
	// it fails with ErrCorrupt, storing nothing, if they don't match.
	Import(ctx context.Context, b Blob, r io.Reader) error
	// Get returns b's content as a stream that verifies it: reading to its end fails with ErrCorrupt
	// if the bytes don't make b's digest and size.
	// A blob found corrupt so is dropped, unless it has been replaced meanwhile,
	// so that Has then reports it missing and the next Put or Import of its content stores it again:
	// a client that captures the same sources again repairs them, though their size was right.
	// The caller must close it.
	Get(ctx context.Context, b Blob) (io.ReadCloser, error)
	// Read returns b's content, verified, or ErrOverLimit if b is larger than limit, reading nothing.
	Read(ctx context.Context, b Blob, limit int64) ([]byte, error)
	// ReadMany returns the content of each of blobs, verified, in order; each must be at most limit bytes.
	ReadMany(ctx context.Context, blobs []Blob, limit int64) ([][]byte, error)
	// ClearContent removes every blob, returning how many there were.
	ClearContent(ctx context.Context) (int, error)
}

// RecordStore holds the latest result of each action and what each tag resolved to.
// Latest means the last write that completed.
// A record that can't be decoded is as good as none: the next run replaces it.
// Keys are SHA-256 digests, as action keys are.
type RecordStore interface {
	// Result returns the latest result for key, or nil if there is none.
	Result(ctx context.Context, key digest.Digest) (*ActionResult, error)
	// SetResult records r as the latest result for key, replacing any earlier one.
	SetResult(ctx context.Context, key digest.Digest, r *ActionResult) error
	// Tag returns the record of the tag name, or nil if there is none.
	// Records are kept until replaced, even by ClearResults.
	Tag(ctx context.Context, name string) (*TagRecord, error)
	// SetTag records r for the tag name, replacing any earlier record.
	SetTag(ctx context.Context, name string, r *TagRecord) error
	// ClearResults removes every action result, but no tag record, returning how many there were.
	ClearResults(ctx context.Context) (int, error)
}

// verifier is a stream of a blob's content that checks, at its end, that the bytes made the blob's digest and size,
// and fails with ErrCorrupt otherwise.
type verifier struct {
	r         io.ReadCloser
	want      Blob
	digester  digest.Digester
	n         int64
	onCorrupt func() // called once the content is found corrupt, if not nil
}

// verify returns r, b's content, as a stream that verifies it, calling onCorrupt, if not nil,
// the first time it finds the content corrupt.
func verify(r io.ReadCloser, b Blob, onCorrupt func()) io.ReadCloser {
	return &verifier{r: r, want: b, digester: digest.Canonical.Digester(), onCorrupt: onCorrupt}
}

// Read reads from the stream, verifying it once it ends.
func (v *verifier) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.digester.Hash().Write(p[:n])
	v.n += int64(n)
	if v.n > v.want.Size {
		return n, v.corrupt(fmt.Sprintf("it holds more than its %d bytes", v.want.Size))
	}
	if errors.Is(err, io.EOF) {
		if v.n != v.want.Size {
			return n, v.corrupt(fmt.Sprintf("it holds %d bytes, not %d", v.n, v.want.Size))
		}
		if got := v.digester.Digest(); got != v.want.Digest {
			return n, v.corrupt("its content has digest " + got.String())
		}
	}
	return n, err
}

// corrupt returns the error saying the content is corrupt for reason, calling onCorrupt the first time.
func (v *verifier) corrupt(reason string) error {
	if v.onCorrupt != nil {
		v.onCorrupt()
		v.onCorrupt = nil
	}
	return Corrupt(v.want, reason)
}

// Close closes the stream.
func (v *verifier) Close() error {
	return v.r.Close()
}

// readAll returns the content of b from store, verified, or ErrOverLimit if b is larger than limit.
// Implementations' Read methods share it.
func readAll(ctx context.Context, store ContentStore, b Blob, limit int64) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if b.Size > limit {
		return nil, fmt.Errorf("blob %s holds %d bytes, %w: %d", b.Digest, b.Size, ErrOverLimit, limit)
	}
	r, err := store.Get(ctx, b)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// readMany returns the content of each of blobs from store, as ReadMany does.
func readMany(ctx context.Context, store ContentStore, blobs []Blob, limit int64) ([][]byte, error) {
	contents := make([][]byte, len(blobs))
	for i, b := range blobs {
		data, err := store.Read(ctx, b, limit)
		if err != nil {
			return nil, err
		}
		contents[i] = data
	}
	return contents, nil
}

// missingOf returns which of blobs store lacks or holds corrupt, as Missing does.
func missingOf(ctx context.Context, store ContentStore, blobs []Blob) ([]Blob, error) {
	var missing []Blob
	for _, b := range blobs {
		err := store.Has(ctx, b)
		switch {
		case errors.Is(err, ErrMissing), errors.Is(err, ErrCorrupt):
			missing = append(missing, b)
		case err != nil:
			return nil, err
		}
	}
	return missing, nil
}

// limited reads at most limit+1 bytes of r, so that a put of more than limit bytes is seen as over the limit
// without reading on.
func limited(r io.Reader, limit int64) io.Reader {
	return io.LimitReader(r, limit+1)
}
