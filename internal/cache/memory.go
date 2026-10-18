package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/opencontainers/go-digest"
)

// Memory is a content store and record store in memory, for tests.
// It keeps the same contract as Directory, and is safe for concurrent use.
type Memory struct {
	mu      sync.Mutex
	blobs   map[digest.Digest][]byte
	results map[digest.Digest][]byte // encoded, as a store would hold them
	tags    map[string][]byte
	cleared uint64 // how many times the results were cleared
}

var (
	_ ContentStore = (*Memory)(nil)
	_ RecordStore  = (*Memory)(nil)
)

// NewMemory returns empty stores in memory.
func NewMemory() *Memory {
	return &Memory{
		blobs:   map[digest.Digest][]byte{},
		results: map[digest.Digest][]byte{},
		tags:    map[string][]byte{},
	}
}

// Replace makes the store hold data under b's digest, whatever data is, so that tests can corrupt a blob.
func (m *Memory) Replace(b Blob, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[b.Digest] = data
}

// Has reports whether the store holds b.
func (m *Memory) Has(_ context.Context, b Blob) error {
	if err := b.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[b.Digest]
	switch {
	case !ok:
		return missing(b)
	case int64(len(data)) != b.Size:
		return Corrupt(b, fmt.Sprintf("it holds %d bytes, not %d", len(data), b.Size))
	}
	return nil
}

// Missing returns which of blobs the store lacks or holds corrupt.
func (m *Memory) Missing(ctx context.Context, blobs []Blob) ([]Blob, error) {
	return missingOf(ctx, m, blobs)
}

// Put stores what r holds, up to limit bytes.
func (m *Memory) Put(_ context.Context, r io.Reader, limit int64) (Blob, error) {
	data, err := io.ReadAll(limited(r, limit))
	if err != nil {
		return Blob{}, err
	}
	if int64(len(data)) > limit {
		return Blob{}, fmt.Errorf("%w: %d", ErrOverLimit, limit)
	}
	b := Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[b.Digest] = data
	return b, nil
}

// PutBytes stores data.
func (m *Memory) PutBytes(ctx context.Context, data []byte) (Blob, error) {
	return m.Put(ctx, bytes.NewReader(data), int64(len(data)))
}

// Import stores what r holds as b, if it is b's content.
func (m *Memory) Import(_ context.Context, b Blob, r io.Reader) error {
	if err := b.Validate(); err != nil {
		return err
	}
	data, err := io.ReadAll(limited(r, b.Size))
	if err != nil {
		return err
	}
	if got := (Blob{Digest: digest.FromBytes(data), Size: int64(len(data))}); got != b {
		return Corrupt(b, fmt.Sprintf("the bytes imported make blob %s of %d bytes", got.Digest, got.Size))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[b.Digest] = data
	return nil
}

// Get returns b's content as a stream that verifies it.
func (m *Memory) Get(_ context.Context, b Blob) (io.ReadCloser, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	data, ok := m.blobs[b.Digest]
	m.mu.Unlock()
	if !ok {
		return nil, missing(b)
	}
	return verify(io.NopCloser(bytes.NewReader(data)), b, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if current, ok := m.blobs[b.Digest]; ok && bytes.Equal(current, data) {
			delete(m.blobs, b.Digest)
		}
	}), nil
}

// Read returns b's content, verified, if it is at most limit bytes.
func (m *Memory) Read(ctx context.Context, b Blob, limit int64) ([]byte, error) {
	return readAll(ctx, m, b, limit)
}

// ReadMany returns the content of each of blobs, verified.
func (m *Memory) ReadMany(ctx context.Context, blobs []Blob, limit int64) ([][]byte, error) {
	return readMany(ctx, m, blobs, limit)
}

// ClearContent removes every blob.
func (m *Memory) ClearContent(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.blobs)
	m.blobs = map[digest.Digest][]byte{}
	return n, nil
}

// decodeRecord decodes data into v, reporting false if it can't, or v isn't valid, which is as good as no record.
func decodeRecord(data []byte, v interface{ Validate() error }) bool {
	return data != nil && int64(len(data)) <= MaxRecordSize && json.Unmarshal(data, v) == nil && v.Validate() == nil
}

// Result returns the latest result for key, or nil.
func (m *Memory) Result(_ context.Context, key digest.Digest) (*ActionResult, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	m.mu.Lock()
	data := m.results[key]
	m.mu.Unlock()
	var r ActionResult
	if !decodeRecord(data, &r) {
		return nil, nil
	}
	return &r, nil
}

// SetResult records r as the latest result for key.
func (m *Memory) SetResult(_ context.Context, key digest.Digest, r *ActionResult) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[key] = data
	return nil
}

// Tag returns the record of the tag name, or nil.
func (m *Memory) Tag(_ context.Context, name string) (*TagRecord, error) {
	m.mu.Lock()
	data := m.tags[name]
	m.mu.Unlock()
	var r TagRecord
	if !decodeRecord(data, &r) {
		return nil, nil
	}
	return &r, nil
}

// SetTag records r for the tag name.
func (m *Memory) SetTag(_ context.Context, name string, r *TagRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tags[name] = data
	return nil
}

// ClearResults removes every action result, but no tag record.
func (m *Memory) ClearResults(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.results)
	m.results = map[digest.Digest][]byte{}
	m.cleared++
	return n, nil
}

// Generation returns how many times the results were cleared.
func (m *Memory) Generation(context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleared, nil
}
