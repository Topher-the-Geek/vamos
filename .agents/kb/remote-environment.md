# Remote execution environment

Remote execution runs builds on a coordinator and workers that share a cache registry and a pull-through cache.
[remote/compose.yaml](../../remote/compose.yaml) runs that environment with Docker Compose in the devcontainer,
so that each step of remote execution is tested against real registries.
The file runs the coordinator, two workers, both built from the release [Dockerfile](../../Dockerfile), and the
registries.
The coordinator is `vamos coordinator`, [coord](../../internal/coord/doc.go) served over HTTP through
[remote](../../internal/remote/doc.go): `remote.Server` serves `api.Executor` and `cache.ContentStore` to clients,
whose stub `remote.Client` reconnects and submits idempotently, and the worker-facing API to the workers,
`vamos worker` from [worker](../../internal/worker/doc.go), each running attempts in contained subprocesses.
Workers push built layers to `registry:5000/vamos-cache` and pull bases through `hub-proxy:5000`.
A client in the devcontainer names the coordinator with `--coordinator http://coordinator:8080` once it has joined
the network; `docker compose up --scale worker=3` runs more workers.
Run the commands below from the repository root.

The registries are plain CNCF Distribution registries, nothing vamos-specific; the services are:

| Service | Role | Notes |
| --- | --- | --- |
| `coordinator` | Schedules what clients submit across the workers, keeps the shared build cache, checks hits against the cache registry | Not privileged; a volume for `/var/lib/vamos`; its registry settings are the compose file's inline copy of `remote/registries.conf`, since a devcontainer path can't be mounted |
| `worker` | Runs attempts in contained subprocesses, pushes layers to the cache registry, pulls bases through the proxy | Privileged, as buildah needs; an anonymous volume for its container store, a disposable cache; two replicas |
| `registry` | The cache registry, which holds built layers tagged `<repository>:<image ID>` | Deletion enabled, so a prune can remove tags; blob space returns only when the registry's garbage collection runs |
| `hub-proxy` | The pull-through cache for Docker Hub, which base images are pulled through | Proxy mode mirrors one upstream and refuses pushes (405); upstream credentials belong on the proxy |

Nothing publishes a port: the devcontainer can't reach ports published on the host,
so a client joins the project's network and reaches the services by name, `registry:5000` and `hub-proxy:5000`.
Both speak plain HTTP without authentication, which suits a trusted network alone:
anyone who can reach the proxy reads whatever its upstream credentials can.
[remote/registries.conf](../../remote/registries.conf) marks them insecure for containers/image;
the services get the same settings as an inline compose config, and a client in the devcontainer points
`CONTAINERS_REGISTRIES_CONF` at the file, which governs the client's own registry access as well as the local
executor's, so `--tls-verify=false` isn't needed besides.
The client needs them to deliver: `load` and `save` read built images from `registry:5000` and pulled bases from
`hub-proxy:5000`, as the results locate them, through [finish](../../internal/finish/doc.go)'s `Image`.

```sh
docker compose -f remote/compose.yaml up -d --build --wait  # the project is named "remote"
docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"
curl -s http://registry:5000/v2/                             # {} once the registry answers
./vamos build server -C examples/typescript-mcp --coordinator http://coordinator:8080
docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"
docker compose -f remote/compose.yaml down -v                # -v drops the registries' contents too
```

The project name is the same from every worktree, so compose treats the environment of one worktree
and another as the same project: run it from one worktree at a time, or pass `-p` to name a second.
Each `up` after a `down -v` pulls base images through the proxy anew, which Docker Hub counts
against its anonymous pull allowance; a `down` without `-v` keeps the registries' contents but also leaves the
workers' anonymous container-store volumes behind, one per worker per `up`, so prefer `down -v` unless the next
run should find the registries warm, and `docker volume prune` now and then.
The coordinator offers `${VAMOS_PLATFORM:-linux/amd64}`: on an arm64 host, export `VAMOS_PLATFORM=linux/arm64`
before `up`, or a client on that host builds for a platform the workers never serve and waits.

Fault injection uses Docker alone:
a worker is lost with `docker restart -t 0`, which kills its process and brings it back, since Docker's restart
policy ignores a `docker kill`; a client is disconnected with `docker network disconnect`; and the coordinator
restarts with `docker restart`.
The checks themselves are in [smoke-checks.md](smoke-checks.md).
