package internal

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/afero"
	"go.podman.io/image/v5/docker/reference"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// buildSpec is the image description declared by a call to build() in a BUILD file.
type buildSpec struct {
	base        string
	sources     []string
	destination string
	run         []string
	command     []string
}

// loadBuildFile executes dir/BUILD from fsys, which must call build() exactly once.
// Sources named by build() must exist within dir.
func loadBuildFile(fsys afero.Fs, dir string) (*buildSpec, error) {
	info, err := fsys.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("directory %s does not exist", dir)
	} else if err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	buildPath := filepath.Join(dir, "BUILD")
	src, err := afero.ReadFile(fsys, buildPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no BUILD file in %s", dir)
	} else if err != nil {
		return nil, err
	}

	var spec *buildSpec
	build := starlark.NewBuiltin("build", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		if spec != nil {
			return nil, fmt.Errorf("%s: may only be called once", fn.Name())
		}
		var base, destination string
		var sources, run, command *starlark.List
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"base", &base,
			"sources?", &sources,
			"destination?", &destination,
			"run?", &run,
			"command?", &command,
		); err != nil {
			return nil, err
		}

		s := &buildSpec{base: base, destination: destination}
		if err := checkBaseImage(base); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		var err error
		if s.sources, err = stringList(fn.Name(), "sources", sources); err != nil {
			return nil, err
		}
		if s.run, err = stringList(fn.Name(), "run", run); err != nil {
			return nil, err
		}
		if s.command, err = stringList(fn.Name(), "command", command); err != nil {
			return nil, err
		}
		if len(s.sources) > 0 && s.destination == "" {
			return nil, fmt.Errorf("%s: destination is required when sources are given", fn.Name())
		}
		if s.destination != "" && !path.IsAbs(s.destination) {
			return nil, fmt.Errorf("%s: destination %q must be an absolute path", fn.Name(), s.destination)
		}
		for _, src := range s.sources {
			if err := checkSource(fsys, dir, src); err != nil {
				return nil, fmt.Errorf("%s: %w", fn.Name(), err)
			}
		}
		spec = s
		return starlark.None, nil
	})

	thread := &starlark.Thread{Name: "BUILD"}
	predeclared := starlark.StringDict{"build": build}
	if _, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, buildPath, src, predeclared); err != nil {
		if evalErr, ok := err.(*starlark.EvalError); ok {
			return nil, errors.New(describeEvalError(evalErr))
		}
		return nil, err
	}
	if spec == nil {
		return nil, fmt.Errorf("%s: build() was never called", buildPath)
	}
	return spec, nil
}

// describeEvalError reports e at the innermost position within a BUILD file, in the
// one-line "file:line:col: message" form of compiler errors.
func describeEvalError(e *starlark.EvalError) string {
	for i := len(e.CallStack) - 1; i >= 0; i-- {
		if pos := e.CallStack[i].Pos; pos.Filename() != "<builtin>" && pos.IsValid() {
			return fmt.Sprintf("%s: %s", pos, e.Msg)
		}
	}
	return e.Msg
}

// checkBaseImage reports whether base names an image that could be pulled.
func checkBaseImage(base string) error {
	if base == "" {
		return errors.New("base image must not be empty")
	}
	if base == "scratch" {
		return nil
	}
	if _, err := reference.ParseNormalizedNamed(base); err != nil {
		return fmt.Errorf("invalid base image %q: %w", base, err)
	}
	return nil
}

// checkSource reports whether src is a relative path to a file or directory within dir.
func checkSource(fsys afero.Fs, dir, src string) error {
	if filepath.IsAbs(src) {
		return fmt.Errorf("source %q must be a path relative to the BUILD file", src)
	}
	if clean := filepath.Clean(src); clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("source %q is outside %s", src, dir)
	}
	// Check each component: lstat on the final path still follows parent links.
	current := dir
	for _, component := range strings.Split(filepath.Clean(src), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, _, err := lstat(fsys, current)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("source %q not found in %s", src, dir)
		} else if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if current == filepath.Join(dir, src) {
				return fmt.Errorf("source %q is a symbolic link; list the file it links to instead", src)
			}
			return fmt.Errorf("source %q has symbolic link parent %q", src, current)
		}
	}
	return nil
}

// lstat describes the file at path in fsys without following a final symbolic link,
// if fsys supports links.
func lstat(fsys afero.Fs, path string) (fs.FileInfo, bool, error) {
	if l, ok := fsys.(afero.Lstater); ok {
		return l.LstatIfPossible(path)
	}
	info, err := fsys.Stat(path)
	return info, false, err
}

// stringList converts an optional Starlark list into a []string.
func stringList(fnName, param string, list *starlark.List) ([]string, error) {
	if list == nil {
		return nil, nil
	}
	out := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		s, ok := starlark.AsString(list.Index(i))
		if !ok {
			return nil, fmt.Errorf("%s: %s[%d] is %s, want string", fnName, param, i, list.Index(i).Type())
		}
		out = append(out, s)
	}
	return out, nil
}
