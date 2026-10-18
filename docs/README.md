# vamos user guide

Start with the [tutorial](tutorial.md).
It introduces images and filesets, sets up vamos, and builds the
[TypeScript UI example](../examples/typescript-ui/README.md) into a running container.

Use these references when writing your own project:

| Reference | Covers |
| --- | --- |
| [BUILD language](reference/build.md) | Targets, steps, and selecting files |
| [CLI](reference/cli.md) | Building, publishing, loading, saving, and exporting |
| [Configuration](reference/configuration.md) | Workspaces, parameters, platforms, and shared `.star` files |
| [Cache and traces](reference/cache.md) | Reuse, rebuilding, timestamps, pruning, and inspecting a build |

See the [examples](../examples/README.md) for complete projects and the build patterns they demonstrate.

## Remote builds

By default vamos builds in the process you run it in, with its own build cache and container store.
A team can share one build cache and a pool of build machines instead: a [coordinator](reference/cli.md#coordinator)
schedules builds across [workers](reference/cli.md#worker) that register with it, and any command that builds takes
`--coordinator http://host:8080`, or `VAMOS_COORDINATOR`, to build there.
Targets already built by anyone are reused, independent targets of one build run on different workers at once,
and the images come back to the client from a cache registry that the workers push to.

Run a coordinator only on a trusted network: it has no authentication, so anyone who can reach it can build on it
and read its cache, and anyone who can reach a pull-through cache reads what its upstream credentials can.
The client must reach the coordinator, the cache registry and, for images that are plain `pull()`s, the pull-through
cache, with its own registry credentials; `--tls-verify=false` or a `registries.conf` entry allows a registry that
speaks plain HTTP.
A build runs its commands on the platform the coordinator's workers have; see
[platforms](reference/configuration.md#platforms) for how that platform is chosen and what happens when the image
being built is for another one.
The `remote/` directory holds a Docker Compose file that runs the whole arrangement on one machine, for trying it.
`remote/helm/vamos` is a Helm chart that deploys it on Kubernetes, documented by its values file.
