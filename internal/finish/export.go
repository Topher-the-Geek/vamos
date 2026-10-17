package finish

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
)

// The reasons export refuses a destination.
var (
	errNotEmpty = errors.New("it isn't empty; pass --replace to replace it")
	errNotDir   = errors.New("it isn't a directory")
)

// removeAll removes what an export replaced.
// Tests replace it, to make it fail.
var removeAll = os.RemoveAll

// exchange swaps the names a and b in one step.
// Tests replace it, to make it fail.
var exchange = defaultExchange

// defaultExchange swaps the names a and b with renameat2(RENAME_EXCHANGE).
func defaultExchange(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}

// CheckExportDest reports, before anything is built, if export can't write a task's files to dir:
// if its parent folder is missing or can't be written,
// or dir is a file or link, or holds anything and replace isn't set.
// dir may change before the files are installed, so Export decides by what it holds then.
func CheckExportDest(dir string, replace bool) error {
	parent := filepath.Dir(dir)
	if info, err := os.Stat(parent); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("directory %s does not exist", parent)
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", parent)
	}
	staged, err := makeStaging(dir)
	if err != nil {
		return err
	}
	os.Remove(staged)

	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return errNotDir
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if names, _ := f.Readdirnames(1); len(names) > 0 && !replace {
		return errNotEmpty
	}
	return nil
}

// Export writes the files of exp, a task's, to dir, from the content store, replacing what dir holds if replace is set,
// and calls warn if what it replaced can't be removed.
// A blob that is missing or corrupt fails the export; the result that names it stands.
// It stages the files in a new folder beside dir, gives them their times and owners,
// and then installs the folder at dir in one step,
// so that dir holds either what it did or all the task's files, never a mix.
// Files that appear in dir during the build are never removed without replace.
func Export(
	ctx context.Context, store cache.ContentStore, exp api.Exported, dir string, replace bool, warn func(error),
) (err error) {
	if exp.Files == nil {
		return errors.New("the task made no files")
	}
	staged, err := makeStaging(dir)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			os.RemoveAll(staged)
		}
	}()
	if _, err := cache.Checkout(ctx, store, *exp.Files, staged); err != nil {
		return fmt.Errorf("copying the files from the build cache: %w", err)
	}
	if err := setMetadata(staged, time.Unix(exp.Epoch, 0)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	old, err := install(staged, dir, replace)
	if old == staged {
		installed = true // staged holds what was at dir, which mustn't be removed unless it was replaced
	}
	if err != nil {
		return err
	}
	installed = true
	// The files are in place, so the export has succeeded even if what they replaced is left over.
	if old != "" {
		if err := removeAll(old); err != nil {
			warn(fmt.Errorf("removing the files replaced at %s: %w", old, err))
		}
	}
	return nil
}

// makeStaging makes a new, empty folder beside dir, on the same filesystem, to stage files for it in.
// It has the usual mode of a new folder.
func makeStaging(dir string) (string, error) {
	for {
		name := filepath.Join(filepath.Dir(dir), fmt.Sprintf(".%s.vamos-%d", filepath.Base(dir), rand.Uint32()))
		err := os.Mkdir(name, 0o755)
		if errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		return name, nil
	}
}

// setMetadata sets the times of everything within root, root included, to epoch,
// so that the same tree exports the same files,
// and, if vamos runs as root, gives each the owner and group of root's parent folder,
// so that files written into a project belong to its owner, not root.
// Neither follows links: a link may point anywhere on the host, and following it would change a file there.
// Each folder is set after what it holds.
// Changing an owner clears the setuid and setgid bits, so they are set again after.
func setMetadata(root string, epoch time.Time) error {
	uid, gid := -1, -1
	if os.Geteuid() == 0 {
		info, err := os.Stat(filepath.Dir(root))
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	var paths []string
	var modes []fs.FileInfo
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		paths, modes = append(paths, path), append(modes, info)
		return err
	})
	if err != nil {
		return err
	}
	// Not UnixNano, which overflows after 2262.
	ts := unix.Timespec{Sec: epoch.Unix(), Nsec: int64(epoch.Nanosecond())}
	times := []unix.Timespec{ts, ts}
	for i := len(paths) - 1; i >= 0; i-- { // a folder after what it holds
		if uid >= 0 {
			if err := os.Lchown(paths[i], uid, gid); err != nil {
				return err
			}
			if modes[i].Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
				if err := os.Chmod(paths[i], modes[i].Mode()); err != nil {
					return err
				}
			}
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, paths[i], times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("setting the times of %s: %w", paths[i], err)
		}
	}
	return nil
}

// install puts the folder staged in place at dir in one step, and returns where what it replaced is, if anything.
// Without replace, dir must be missing or an empty folder, as the kernel checks as it renames.
// With replace, a folder at dir is swapped with staged, so that dir is never missing,
// and staged then holds the folder; anything else at dir is kept, and refused.
// If what was at dir can't be put back, it is left where install returns.
func install(staged, dir string, replace bool) (string, error) {
	if !replace {
		// Not os.Rename, which refuses to rename onto a folder, even an empty one.
		err := unix.Rename(staged, dir)
		switch {
		case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
			return "", errNotEmpty
		case errors.Is(err, syscall.ENOTDIR):
			return "", errNotDir
		case err != nil:
			return "", &os.LinkError{Op: "rename", Old: staged, New: dir, Err: err}
		}
		return "", nil
	}
	err := exchange(staged, dir)
	switch {
	case errors.Is(err, unix.ENOENT):
		return install(staged, dir, false) // nothing to replace
	case errors.Is(err, unix.EINVAL):
		return swap(staged, dir) // the filesystem can't exchange names
	case err != nil:
		return "", err
	}
	if info, err := os.Lstat(staged); err != nil || !info.IsDir() {
		// Put back what was at dir, which isn't a folder, and keep the files staged, to be removed.
		if err := exchange(staged, dir); err != nil {
			return staged, fmt.Errorf("putting back what was at %s, which is now at %s: %w", dir, staged, err)
		}
		return "", errNotDir
	}
	return staged, nil
}

// swap installs staged at dir as install does with replace, on a filesystem that can't exchange two names in one step.
// It moves the folder at dir aside, then staged into its place, so dir is briefly missing.
func swap(staged, dir string) (string, error) {
	aside, err := makeStaging(dir)
	if err != nil {
		return "", err
	}
	// Renaming onto the empty folder aside succeeds only if dir is a folder too.
	if err := unix.Rename(dir, aside); err != nil {
		os.Remove(aside)
		switch {
		case errors.Is(err, unix.ENOENT):
			return install(staged, dir, false) // nothing to replace
		case errors.Is(err, unix.EISDIR), errors.Is(err, unix.ENOTDIR):
			return "", errNotDir
		}
		return "", &os.LinkError{Op: "rename", Old: dir, New: aside, Err: err}
	}
	if err := unix.Rename(staged, dir); err != nil {
		if err := unix.Rename(aside, dir); err != nil {
			return aside, fmt.Errorf("putting back what was at %s, which is now at %s: %w", dir, aside, err)
		}
		return "", &os.LinkError{Op: "rename", Old: staged, New: dir, Err: err}
	}
	return aside, nil
}
