package cache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/cache/cachetest"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// implementations returns a fixture for each implementation of the stores: the directory on disk, which a test
// corrupts by writing the blob's file, and the stores in memory.
func implementations(t *testing.T) map[string]cachetest.Fixture {
	t.Helper()
	return map[string]cachetest.Fixture{
		"directory": {New: func(t *testing.T) cachetest.Stores {
			dir := t.TempDir()
			d, err := cache.OpenDirectory(dir)
			testenv.Must(t, err)
			t.Cleanup(func() { d.Close() })
			return cachetest.Stores{Content: d, Records: d, Corrupt: func(t *testing.T, b cache.Blob, data []byte) {
				path := filepath.Join(dir, "cas", b.Digest.Algorithm().String(), b.Digest.Encoded())
				testenv.Must(t, os.WriteFile(path, data, 0o600))
			}}
		}},
		"memory": {New: func(t *testing.T) cachetest.Stores {
			m := cache.NewMemory()
			return cachetest.Stores{Content: m, Records: m, Corrupt: func(t *testing.T, b cache.Blob, data []byte) {
				m.Replace(b, data)
			}}
		}},
	}
}

// TestContentStore runs the content store contract on each implementation.
func TestContentStore(t *testing.T) {
	for name, f := range implementations(t) {
		t.Run(name, func(t *testing.T) { cachetest.Content(t, f) })
	}
}

// TestRecordStore runs the record store contract on each implementation.
func TestRecordStore(t *testing.T) {
	for name, f := range implementations(t) {
		t.Run(name, func(t *testing.T) { cachetest.Records(t, f) })
	}
}
