package load

import (
	"fmt"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/fileset"

	"go.starlark.net/starlark"
)

// selectionParams returns the parameters of glob() and task() that make sel, for starlark.UnpackArgs. Its
// include and exclude lists are unpacked into include and exclude, which selectFiles then checks, and the
// rest into sel. include is optional unless required.
func selectionParams(sel *fileset.Selection, include, exclude **starlark.List, required bool) []any {
	includeParam := "include?"
	if required {
		includeParam = "include"
	}
	return []any{
		includeParam, include,
		"exclude?", exclude,
		"mode_mask?", &sel.ModeMask,
		"allow_no_files?", &sel.AllowNoFiles,
		"max_file_count?", &sel.MaxFileCount,
		"max_file_size?", &sel.MaxFileSize,
		"max_total_size?", &sel.MaxTotalSize,
	}
}

// isSelectionParam reports whether name is one of the parameters that selectionParams returns, so that task() can
// tell those given without source from its own.
func isSelectionParam(name string) bool {
	params := selectionParams(fileset.DefaultSelection(), new(*starlark.List), new(*starlark.List), false)
	for i := 0; i < len(params); i += 2 {
		if strings.TrimSuffix(params[i].(string), "?") == name {
			return true
		}
	}
	return false
}

// selectFiles sets sel's patterns from the include and exclude arguments of a call to fnName, and checks
// them, relative to dir.
func selectFiles(sel *fileset.Selection, fnName string, include, exclude *starlark.List, dir string) error {
	var err error
	if sel.Include, err = stringList(fnName, "include", include); err != nil {
		return err
	}
	if len(sel.Include) == 0 {
		return fmt.Errorf("%s: include must not be empty", fnName)
	}
	if sel.Exclude, err = stringList(fnName, "exclude", exclude); err != nil {
		return err
	}
	if err := sel.Compile(dir); err != nil {
		return fmt.Errorf("%s: %w", fnName, err)
	}
	return nil
}
