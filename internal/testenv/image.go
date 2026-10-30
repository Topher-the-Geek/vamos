package testenv

import (
	"archive/tar"
	"errors"
	"io"
	"path"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// WalkImage calls visit with each entry in img's flattened filesystem, and a reader of its content,
// until visit returns true. It reports whether visit did so.
func WalkImage(img v1.Image, visit func(*tar.Header, io.Reader) bool) (bool, error) {
	rc := mutate.Extract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		if visit(hdr, tr) {
			return true, nil
		}
	}
}

// ImagePath returns the absolute path within an image of a layer entry named name in its tar,
// such as "./app/" for "/app", and "." for "/".
func ImagePath(name string) string {
	return path.Clean("/" + name)
}
