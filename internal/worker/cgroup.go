package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// cgroupRoot is where the worker makes a cgroup for each attempt, beneath the cgroup v2 hierarchy,
// so that a kill reaches every process an attempt started, however it detached itself.
const cgroupRoot = "/sys/fs/cgroup/vamos"

// containment is how attempts are contained: cgroups, when the host has cgroup v2 that the worker may write.
type containment struct {
	root string // the cgroup that attempts' cgroups are made in
}

// newContainment checks that attempts can be contained here, making the worker's cgroup, and returns how.
// A worker that can't contain attempts mustn't register: a kill that misses a process leaves it running.
func newContainment(root string) (*containment, error) {
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("attempts can't be contained: the cgroup v2 hierarchy isn't at %s: %w",
			filepath.Dir(root), err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("attempts can't be contained: making the cgroup %s: %w", root, err)
	}
	// A cgroup can hold processes only if it has no children with controllers, or controllers of its own:
	// the worker's cgroup holds none; each attempt's cgroup holds that attempt.
	probe := filepath.Join(root, "probe")
	if err := os.Mkdir(probe, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("attempts can't be contained: making a cgroup in %s: %w", root, err)
	}
	if _, err := os.Stat(filepath.Join(probe, "cgroup.kill")); err != nil {
		os.Remove(probe)
		return nil, fmt.Errorf("attempts can't be contained: the kernel's cgroups have no cgroup.kill: %w", err)
	}
	if err := os.Remove(probe); err != nil {
		return nil, fmt.Errorf("attempts can't be contained: removing a cgroup in %s: %w", root, err)
	}
	return &containment{root: root}, nil
}

// path returns the cgroup of the attempt id.
func (c *containment) path(id string) string {
	return filepath.Join(c.root, "attempt-"+id)
}

// create makes the cgroup for the attempt id and returns it, open, for the attempt's process to be started in.
// The caller closes the file.
func (c *containment) create(id string) (*os.File, error) {
	dir := c.path(id)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("making the attempt's cgroup: %w", err)
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("opening the attempt's cgroup: %w", err)
	}
	return f, nil
}

// kill kills every process in the attempt's cgroup and waits for it to empty, for at most wait,
// then removes the cgroup.
func (c *containment) kill(id string, wait time.Duration) error {
	dir := c.path(id)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("killing the attempt's cgroup: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
		if err != nil {
			return fmt.Errorf("reading the attempt's cgroup: %w", err)
		}
		if strings.TrimSpace(string(procs)) == "" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the attempt's processes didn't exit within %s of being killed: %s", wait,
				strings.Join(strings.Fields(string(procs)), ", "))
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c.remove(id)
}

// remove removes the attempt's cgroup, which must be empty.
func (c *containment) remove(id string) error {
	if err := os.Remove(c.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the attempt's cgroup: %w", err)
	}
	return nil
}

// leftovers returns the IDs of the attempts whose cgroups remain from an earlier run of a worker that crashed.
func (c *containment) leftovers() ([]string, error) {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutPrefix(e.Name(), "attempt-"); ok && e.IsDir() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
