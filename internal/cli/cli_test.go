package cli

import (
	"os"
	"testing"
)

// assertNoFiles fails the test if dir holds anything, such as a partly written image.
func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("unexpected file left in %s: %s", dir, e.Name())
	}
}
