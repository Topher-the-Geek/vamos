package cache_test

import (
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
)

// TestDirectoryGenerationPersists checks that the generation a Directory counts its clears with survives closing
// and reopening it, as a coordinator restart does.
func TestDirectoryGenerationPersists(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	d, err := cache.OpenDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClearResults(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClearResults(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = cache.OpenDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if g, err := d.Generation(ctx); err != nil || g != 2 {
		t.Errorf("generation after reopening = %d, %v; want 2", g, err)
	}
}
