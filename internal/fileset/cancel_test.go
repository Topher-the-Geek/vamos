package fileset

import (
	"context"
	"errors"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/spf13/afero"
)

// TestCancelCapture checks that capturing a project's files stops promptly once the context ends.
func TestCancelCapture(t *testing.T) {
	files := map[string]string{}
	for i := range 50 {
		files["dir"+string(rune('a'+i%26))+"/f"+string(rune('a'+i/26))+".txt"] = "x"
	}
	fsys := testenv.Project(t, files)
	ctx, cancel := context.WithCancel(t.Context())
	opened := 0
	counting := &countingFs{Fs: fsys, open: func() {
		opened++
		if opened == 3 {
			cancel()
		}
	}}
	sel := testSelection(t, []string{"."}, nil, nil)
	_, _, err := Store(ctx, cache.NewMemory(), counting, testenv.ProjectDir, sel)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Store = %v, want %v", err, context.Canceled)
	}
	if opened > 10 {
		t.Errorf("the walk opened %d files after being cancelled early", opened)
	}
}

// countingFs calls open each time a file or directory is opened.
type countingFs struct {
	afero.Fs
	open func()
}

// Open counts, then opens.
func (f *countingFs) Open(name string) (afero.File, error) {
	f.open()
	return f.Fs.Open(name)
}
