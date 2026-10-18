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

## Kubernetes

[remote/helm/vamos](../../remote/helm/vamos/values.yaml) is a Helm chart for the same services on a cluster,
with each object named after the release, as `vamos-coordinator:8080`, `vamos-registry:5000` and
`vamos-hub-proxy:5000` for a release named `vamos`.
Its values file documents every setting; what differs from the compose file is below.

| Object | Notes |
| --- | --- |
| Coordinator | A StatefulSet of one, so that a replacement starts only once the old instance has stopped; its build cache on a volume claim; not root |
| Workers | A Deployment per pool in `workers.pools`, each pool one platform, on nodes of that architecture; the coordinator offers every pool's platform |
| Registries | A StatefulSet each with a volume claim: the cache registry, and a proxy per entry of `pullThrough.caches`; either can be replaced by one elsewhere |
| Registry settings | One ConfigMap, generated from the registries' addresses, as `$CONTAINERS_REGISTRIES_CONF` for the coordinator and the workers |

Kubernetes runs a privileged container in the node's cgroup namespace, unlike Docker,
so `/sys/fs/cgroup` in a worker is the node's whole hierarchy:
workers on one node would share `/sys/fs/cgroup/vamos`, each cleaning up the others' attempts as its own leftovers,
and attempts would run outside the pod's cgroup and the limits set on it.
The worker's command therefore starts it in a cgroup namespace of its own with `unshare --cgroup --mount`,
with cgroup2 mounted over `/sys/fs/cgroup`, which puts the root at its container's cgroup, as Docker does.
The container store is an emptyDir, which outlives a restart of the container but not the pod,
so a worker that crashed cleans up after its attempts, and a deleted pod takes its store with it.

Clients reach the coordinator and the registries by the names above, which resolve only inside the cluster
unless the values give `cacheRegistry.address` and each cache's `address` as names that clients resolve as well.
`vamos build` needs the coordinator alone, which `kubectl port-forward` gives a client outside the cluster;
`load`, `save` and `push --push-from=client` need the registries by the same names the workers use.

The devcontainer has `kind`, `kubectl`, `helm` and `kubeconform`.
kind runs the cluster's node as a container on the host's Docker, on a network named `kind`,
which the devcontainer joins to reach the API server and the node, as it joins the compose network.
A chart under development runs the working tree's release image rather than the published one,
so it is built, loaded into the node as `vamos:latest` and named with `image.repository=vamos`;
on an arm64 host the pool is `linux/arm64`.

```sh
kind create cluster --name vamos                             # once; skip if `kind get clusters` lists it
docker network connect kind "vamos_$(basename "$WORKSPACE_FOLDER")"
kind export kubeconfig --name vamos --internal               # the API server by its name on the kind network
docker build -q . -t vamos:latest && kind load docker-image vamos:latest --name vamos
helm install vamos remote/helm/vamos -n vamos --create-namespace --wait --set image.repository=vamos \
  --set workers.pools.amd64=null --set workers.pools.arm64.platform=linux/arm64   # on an arm64 host
helm test vamos -n vamos
kubectl -n vamos port-forward svc/vamos-coordinator 18080:8080 &
./vamos build server -C examples/typescript-mcp --coordinator http://127.0.0.1:18080
helm uninstall vamos -n vamos && kubectl delete namespace vamos   # the namespace takes the volume claims
```

A worker is lost with `crictl stop --timeout 0` on its container, run in the node with `docker exec`:
the kubelet starts it again in the same pod, as Docker's restart policy does.
A worker's cgroups are seen from its own namespace, `kubectl exec <pod> -- nsenter -t 1 -C -m ls /sys/fs/cgroup/vamos`,
since `kubectl exec` enters the container's other namespaces but not that one.

## Published image

The chart's default image is `ghcr.io/topher-the-geek/vamos`,
which the [release workflow](../../.github/workflows/release.yml) publishes as an index for `linux/amd64` and `linux/arm64`,
since developers and clusters run both.
It runs only when started by hand, never on a push or pull request,
because each run builds the Dockerfile twice and a cluster should take a new image deliberately:
start it from the Actions tab, or with `gh workflow run release.yml --ref <branch> -f tag=<tag>`.
Each run tags the index `sha-<commit>` besides the requested tag, which defaults to `latest`.

Each platform builds natively on a runner of its architecture, `ubuntu-24.04` and `ubuntu-24.04-arm`,
and pushes its image by digest; a final job tags the index of both.
Building both on one runner was rejected: buildah's namespace setup needs cgo,
so the binary can't be cross-compiled without a cross toolchain, and under QEMU the Go build is many times slower.

The image is also how vamos is installed: a project's devcontainer copies `/usr/local/bin/vamos` out of it,
as the [tutorial](../../docs/tutorial.md#in-your-projects-devcontainer) shows, so the binary must run outside Fedora.
The Dockerfile builds it on Debian 12, whose glibc 2.36 is older than Ubuntu 24.04's,
with the `containers_image_openpgp` and `exclude_graphdriver_btrfs` tags, so it links no C library but glibc.
A static build was rejected: with cgo disabled, buildah's chroot isolation fails to start a `run` step.
The devcontainer's own builds, and the test suite, keep gpgme and btrfs, so a change to signature checking
or storage drivers needs a release image smoke check as well.
The workflow pushes with the job's `GITHUB_TOKEN`, so it needs no stored credential.
Pull it locally with `docker pull ghcr.io/topher-the-geek/vamos:<tag>`;
a cluster needs the package to be public, or a `kubernetes.io/dockerconfigjson` Secret in `imagePullSecrets`.
