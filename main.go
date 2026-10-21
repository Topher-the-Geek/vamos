package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Topher-the-Geek/vamos/internal/cli"
	"github.com/spf13/afero"
	"go.podman.io/buildah"
	"go.podman.io/storage/pkg/unshare"
)

func main() {
	if buildah.InitReexec() {
		return
	}
	// A root without the capabilities to use container storage is re-executed in a user namespace that has them;
	// a coordinator, help and --version open no container storage, and may run without any such capability.
	if cli.UsesContainerStore(os.Args[1:]) {
		unshare.MaybeReexecUsingUserNamespace(false)
	}

	// On Ctrl-C or SIGTERM, cancel the build so its deferred cleanup runs:
	// the temporary tarball, the working container and the copied sources.
	// A second signal kills vamos.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()

	if err := cli.NewRootCommand(cli.Config{Fs: afero.NewOsFs()}).ExecuteContext(ctx); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("interrupted: %w", err)
		}
		fmt.Fprintf(os.Stderr, "vamos: %v\n", err)
		os.Exit(1)
	}
}
