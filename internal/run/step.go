package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
)

// targetState is a target being built, which its steps act on in turn.
type targetState struct {
	runner     *localRunner
	builder    *buildah.Builder
	mountPoint string // where the container's files are on the host, mounted while the target runs
	log        *cache.LogRecorder
	events     sched.Output      // where what the target prints goes
	buildEnv   map[string]string // set by build_env(), and not since by env()
	emulated   bool              // a run() step ran under emulation, the image being for another platform
	secrets    map[string][]byte // the values of the secrets that the target's run() steps are given, by ID
}

// lookup returns a function that gives the value of a variable as a run in the target would see it now:
// set by build_env(), or else in the image's config.
// An unset variable is empty.
func (st *targetState) lookup() func(name string) string {
	env := map[string]string{}
	for _, v := range st.builder.Env() {
		name, value, _ := strings.Cut(v, "=")
		env[name] = value
	}
	maps.Copy(env, st.buildEnv)
	return func(name string) string { return env[name] }
}

// expandPath expands the variables in p, a step's argument param, which CheckPath has checked, and returns it cleaned.
// It must then be absolute.
func (st *targetState) expandPath(param, p string) (string, error) {
	expanded, err := graph.ExpandVars(p, st.lookup())
	if err != nil {
		return "", err // CheckPath checks the syntax, so this can't happen
	}
	if !path.IsAbs(expanded) {
		return "", fmt.Errorf("%s %q is %q, which must be an absolute path", param, p, expanded)
	}
	return path.Clean(expanded), nil
}

// giveSecrets gives a run() step its secrets, through options, as buildah's secret mounts: each value is written to
// a file of its own, readable only by this process, in a directory that it returns, for the caller to remove once the
// step has run, even on failure. buildah copies it into the container's own directory, which no image includes,
// and mounts it at the secret's file, or reads it into the secret's variable, for the step alone.
func (st *targetState) giveSecrets(secrets []graph.Secret, options *buildah.RunOptions) (string, error) {
	dir, err := os.MkdirTemp("", "vamos-secrets-")
	if err != nil {
		return "", err
	}
	options.Secrets = map[string]define.Secret{}
	for _, secret := range secrets {
		value, ok := st.secrets[secret.ID]
		if !ok {
			return dir, fmt.Errorf("secret %q isn't given here: secrets reach only an executor in the client's "+
				"process, never a coordinator or its workers", secret.ID)
		}
		if _, ok := options.Secrets[secret.ID]; !ok {
			file := filepath.Join(dir, secret.ID)
			if err := os.WriteFile(file, value, 0o600); err != nil {
				return dir, err
			}
			options.Secrets[secret.ID] = define.Secret{ID: secret.ID, Source: file, SourceType: "file"}
		}
		mount := "type=secret,id=" + secret.ID
		if secret.Env != "" {
			mount += ",env=" + secret.Env
		} else {
			mount += ",target=" + secret.File
		}
		options.RunMounts = append(options.RunMounts, mount)
	}
	return dir, nil
}

// secretIDs returns the IDs of secrets, for a trace, which never holds their values.
func secretIDs(secrets []graph.Secret) []string {
	ids := make([]string, len(secrets))
	for i, secret := range secrets {
		ids[i] = secret.ID
	}
	return ids
}

// tracedApply applies the step in, as a span of ctx's trace, named display, as the BUILD file wrote the step.
func (st *targetState) tracedApply(ctx context.Context, in sched.Step, display string) (err error) {
	ctx, span := tracing.Start(ctx, display, "op", in.Op)
	defer func() { span.EndWith(err) }()
	return st.apply(ctx, in)
}

// apply performs the step in on the target being built, recording what it did in ctx's span.
func (st *targetState) apply(ctx context.Context, in sched.Step) error {
	span := tracing.Current(ctx)
	switch in.Op {
	case graph.OpAdd:
		if in.Files == nil {
			return errors.New("its files are missing from the target's action") // the scheduler always stores them
		}
		dest, err := st.expandPath("dest", in.Dest)
		if err != nil {
			return err
		}
		span.Set("tree", in.Files.Digest.String(), "dest", dest, "owner", chown(in.Owner, in.Group))
		span.Use(graph.TreeFlow(*in.Files))
		return st.addSource(ctx, *in.Files, dest, chown(in.Owner, in.Group))

	case graph.OpRun:
		// Variables in the image's config are passed too; these override them.
		var env []string
		for _, name := range slices.Sorted(maps.Keys(st.buildEnv)) {
			env = append(env, name+"="+st.buildEnv[name])
		}
		stdout := st.log.Writer(cache.StreamStdout, sched.StreamWriter{Out: st.events, Stream: cache.StreamStdout})
		stderr := st.log.Writer(cache.StreamStderr, sched.StreamWriter{Out: st.events, Stream: cache.StreamStderr})
		if tracing.Enabled(ctx) {
			span.Set("argv", in.Argv)
			if p := builderPlatform(st.builder); !platforms.NewMatcher(HostPlatform).Match(p) {
				span.Set("emulated", platforms.Format(p))
			}
			var outCount, errCount countingWriter
			defer func() { span.Set("stdout bytes", outCount.n, "stderr bytes", errCount.n) }()
			stdout, stderr = io.MultiWriter(&outCount, stdout), io.MultiWriter(&errCount, stderr)
		}
		options := buildah.RunOptions{
			Isolation: st.runner.isolation,
			Terminal:  buildah.WithoutTerminal,
			Env:       env,
			Stdout:    stdout,
			Stderr:    stderr,
		}
		if len(in.Secrets) > 0 {
			dir, err := st.giveSecrets(in.Secrets, &options)
			if dir != "" {
				defer os.RemoveAll(dir)
			}
			if err != nil {
				return err
			}
			span.Set("secrets", secretIDs(in.Secrets))
		}
		return st.builder.Run(in.Argv, options)

	case graph.OpEnv, graph.OpBuildEnv:
		// As in a Dockerfile's ENV, every value refers to the variables as they were before the call.
		// They are set in the order actionEntry gives, sorted, so that targets with the same key make the same image.
		lookup := st.lookup()
		var values []string // for the span, if traced
		traced := tracing.Enabled(ctx)
		if traced {
			defer func() { span.Set("values", values) }()
		}
		for _, v := range in.Vars {
			name, value, _ := strings.Cut(v, "=")
			value, err := graph.ExpandVars(value, lookup)
			if err != nil {
				return err // newEnv checks the syntax, so this can't happen
			}
			if traced {
				values = append(values, name+"="+value)
			}
			if in.Op == graph.OpBuildEnv {
				st.buildEnv[name] = value
			} else {
				st.builder.SetEnv(name, value)
				delete(st.buildEnv, name) // which would otherwise override the newer value
			}
		}
		return nil

	case graph.OpWorkdir:
		dir, err := st.expandPath("path", in.Dir)
		if err != nil {
			return err
		}
		span.Set("dir", dir)
		// As a Dockerfile's WORKDIR does, a new directory belongs to the image's user, so later runs can write to it.
		// buildah works within the container's root, so a link in the image can't lead outside it.
		mode := os.FileMode(0o755)
		if err := st.builder.EnsureContainerPathAs(dir, st.builder.User(), &mode); err != nil {
			return err
		}
		st.builder.SetWorkDir(dir)
		return nil
	}
	return fmt.Errorf("unknown step %q", in.Op)
}

// countingWriter counts the bytes written to it, and discards them.
type countingWriter struct {
	n int64
}

// Write counts data.
func (w *countingWriter) Write(data []byte) (int, error) {
	w.n += int64(len(data))
	return len(data), nil
}

// chown returns the owner of an add()'s files, given its owner and group,
// as buildah's Chown option takes it, or "" for root.
func chown(owner, group string) string {
	switch {
	case group == "":
		return owner
	case owner == "":
		return "0:" + group
	}
	return owner + ":" + group
}
