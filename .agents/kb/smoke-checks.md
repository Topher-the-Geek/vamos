# Manual smoke checks

Run from the repository root in the [development environment](development.md).
Required change scopes and other validation are in [testing.md](testing.md).
Create a temporary directory for the tarballs and traces:

```sh
TMP=$(mktemp -d)
```

The MCP checks send `examples/typescript-mcp/echo-request.jsonl` over stdin and should end with its reply:
`{"result":{"content":[{"type":"text","text":"hola"}]},"jsonrpc":"2.0","id":2}`.

```
# 1. Command line: help works; mistakes print `vamos: ...` and exit 1
go build -o vamos . && ./vamos --help && ./vamos nope; ./vamos load

# 2. load into Docker, then run
./vamos load server vamos-example -C examples/typescript-mcp \
  && docker run -i --rm vamos-example < examples/typescript-mcp/echo-request.jsonl

# 3. save a tarball, then load and run it (use a temp dir, not the repo)
./vamos save server "$TMP/example.tar" -t vamos-example:tar -C examples/typescript-mcp \
  && docker load -i "$TMP/example.tar" && docker run -i --rm vamos-example:tar \
  < examples/typescript-mcp/echo-request.jsonl

# 4. The release image.
# Paths in the devcontainer aren't host paths, so pipe the example in instead of bind-mounting it.
docker build -q . -t vamos \
  && tar -C examples/typescript-mcp --exclude=node_modules -c . | docker run -i --rm --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock --entrypoint sh vamos \
  -c 'mkdir /example && tar -x -C /example && vamos load server vamos-example:image -C /example' \
  && docker run -i --rm vamos-example:image < examples/typescript-mcp/echo-request.jsonl

# 5. The fastmcp example.
# Its SDK cancels pending requests when stdin closes, so hold stdin open long enough for it to start.
./vamos load server vamos-fastmcp -C examples/fastmcp \
  && (cat examples/typescript-mcp/echo-request.jsonl; sleep 6) | docker run -i --rm vamos-fastmcp

# 6. The strawberry-react workspace, behind its compose file's ingress.
# The devcontainer can't reach the host's port 8080, so curl from the compose network.
# Expect the page's <title>, then the reply below.
S=examples/strawberry-react
./vamos load //backend:layer strawberry-backend -C $S && ./vamos load //ui:layer strawberry-ui -C $S \
  && docker compose -f $S/docker-compose.yaml up -d --wait \
  && curl="docker run --rm --network strawberry-graphql_default curlimages/curl -s" \
  && $curl http://ingress/ | grep '<title>' \
  && $curl http://ingress/graphql -H 'content-type: application/json' \
  -d '{"query":"mutation { increment { value } }"}'; \
  docker compose -f $S/docker-compose.yaml down
```

Check 6's reply is `{"data":{"increment":{"value":1}}}`.

```
# 7. The golang-cli example: an index, cross-compiled; load sends the daemon's platform's image.
# Expect "hola", from each platform's image.
./vamos load echo vamos-echo -C examples/golang-cli && docker run --rm vamos-echo \
  && ./vamos save //:layer "$TMP/echo.tar" -t vamos-echo:other --platform linux/amd64 -C examples/golang-cli \
  && docker load -i "$TMP/echo.tar" && docker run --rm --platform linux/amd64 vamos-echo:other
```

```
# 8. export: the strawberry-react UI's generated GraphQL types, written to a temp dir, then again over them.
# Expect src/gql's files, owned by you, and "it isn't empty" from the second, until --replace.
S=examples/strawberry-react
./vamos export //ui:graphql-codegen "$TMP/gql" -C $S && ls -ln "$TMP/gql" \
  && ./vamos export //ui:graphql-codegen "$TMP/gql" -C $S; ./vamos export //ui:graphql-codegen "$TMP/gql" \
  --replace -C $S
```

```
# 9. A trace, queried with Perfetto's trace processor, which the devcontainer has.
# Expect the slowest spans, then arrows from what each glob, pull or target made to the steps that use it:
# none if every target was reused.
./vamos load server vamos-example -C examples/typescript-mcp --trace "$TMP/trace.json" \
  && echo "select name, dur/1000000 as ms from slice order by dur desc limit 10;
           select s1.name, s2.name from flow join slice s1 on slice_out = s1.id join slice s2 on slice_in = s2.id;" \
  | trace_processor_shell -q /dev/stdin "$TMP/trace.json"
```

```sh
# 10. The introductory tutorial's TypeScript UI: build, load, serve, and export.
# Expect the heading and bundled message over HTTP, matching the exported website.
# The script checks inside the container, so it also works from the devcontainer.
./examples/typescript-ui/check.sh
```

`push` has no manual check here:
the tests already push to a real (in-process) registry, from the executor and from the client.

## Remote execution

The [remote environment](remote-environment.md) runs the coordinator, two workers and the registries that remote
builds share.

```sh
# R1. The registries come up, answer by name from the devcontainer, and the pull-through cache serves Docker Hub.
# Expect two 200s, then 405: the proxy refuses pushes.
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && curl -s -o /dev/null -w 'registry %{http_code}\n' http://registry:5000/v2/ \
  && curl -s -o /dev/null -w 'hub-proxy %{http_code}\n' \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
    http://hub-proxy:5000/v2/library/busybox/manifests/1.36 \
  && curl -s -X POST -o /dev/null -w 'hub-proxy push %{http_code}\n' http://hub-proxy:5000/v2/x/blobs/uploads/; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# R2. A cold build on the coordinator, loaded into Docker from the cache registry, then a warm one: expect the MCP
# reply, then every target cached. The client reads images from the registry over plain HTTP, which its registry
# settings must allow. The images are built from the Dockerfile, so this takes a few minutes the first time.
export CONTAINERS_REGISTRIES_CONF=$PWD/remote/registries.conf
docker compose -f remote/compose.yaml up -d --build --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && ./vamos load server vamos-example:remote -C examples/typescript-mcp --coordinator http://coordinator:8080 \
  && docker run -i --rm vamos-example:remote < examples/typescript-mcp/echo-request.jsonl \
  && ./vamos build server -C examples/typescript-mcp --coordinator http://coordinator:8080; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# A coordinator refuses a prune for a lease (30s by default) after it starts, and while any build is held by a
# client that hasn't released it; a prune in these checks waits that long after `up`.
# The fault checks need a target that runs long enough to interrupt: with the proxy warm, npm install takes a
# second, so they use a project of their own whose layer sleeps, and leaves a daemon behind with setsid.
mkdir -p "$TMP/slow" && printf '%s\n' \
  'pull(name="busybox", image="docker.io/library/busybox", tag="1.36")' \
  'layer(name="slow", base="busybox", steps=[run(["sh", "-c", "setsid sleep 300 & sleep 45"])])' \
  > "$TMP/slow/BUILD"

# R3. A dropped connection: take this container off the coordinator's network while a build runs, for longer than
# the client's minute-long wait for a line of the event stream, and expect warnings that the connection dropped and
# came back, then the build's success: the worker finished the target meanwhile, and the resumed watch finds it.
# A blip shorter than that wait proves nothing, since the stream's TCP connection survives it.
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && (sleep 10; docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; sleep 75; \
      docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")") & \
  ./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://coordinator:8080; wait; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# R4. A worker killed mid-build: the build's running target is tried again on the other worker and the build
# succeeds, its trace naming the retry; the worker comes back, logs that it registered again, and cleans up after
# the attempt it was running. `docker restart -t 0` kills the process at once and starts the container again;
# `docker kill` would leave it stopped, since Docker's restart policy ignores a manual kill. Workers take work as
# they poll, so the attempt may be on worker-2: if the trace names no retry, run the check again restarting that one.
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && (sleep 15; docker restart -t 0 remote-worker-1) & \
  ./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://coordinator:8080 --trace "$TMP/r4.json"; wait; \
  grep -o '"retry 1"' "$TMP/r4.json"; \
  docker compose -f remote/compose.yaml logs worker | grep -i "registered\|cleaning up"; \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down

# R5. Ctrl-C kills the running attempt on the worker, and every process it started: expect no attempt cgroup
# left on either worker a few seconds after interrupting the build (the release image has no ps; the cgroup
# directory under /sys/fs/cgroup/vamos is the attempt, and it is removed once its processes are gone).
docker compose -f remote/compose.yaml up -d --wait \
  && docker network connect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")" \
  && (./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://coordinator:8080 & \
      sleep 15; kill -INT $!; wait; sleep 5; \
      for i in 1 2; do docker compose -f remote/compose.yaml exec --index $i worker ls /sys/fs/cgroup/vamos; done); \
  docker network disconnect remote_default "vamos_$(basename "$WORKSPACE_FOLDER")"; \
  docker compose -f remote/compose.yaml down
```

## Kubernetes

The Helm chart's checks run on the kind cluster of the [remote environment](remote-environment.md#kubernetes),
installed as that section shows, with the slow project of the checks above and the coordinator forwarded to
`127.0.0.1:18080`. Run each check's lines as one script: a client started in the background of one shell and watched
from another is easily killed with it.

```sh
# K1. The chart renders, and what it renders is valid Kubernetes; expect no errors and every resource valid.
helm lint --strict remote/helm/vamos \
  && helm template vamos remote/helm/vamos | kubeconform -strict -summary

# K2. A worker killed mid-build, as R4: the build succeeds with a retry, and the worker comes back in the same pod,
# cleaning up after its attempt. Each worker has a cgroup namespace of its own, so the attempt's cgroup is in the
# pod that runs it alone, which picks the victim.
./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://127.0.0.1:18080 --trace "$TMP/k2.json" & B=$!
sleep 15
for p in $(kubectl -n vamos get pods -o name | grep worker); do
  kubectl -n vamos exec $p -- nsenter -t 1 -C -m ls /sys/fs/cgroup/vamos | grep -q attempt && V=$p
done
docker exec vamos-control-plane crictl stop --timeout 0 \
  "$(kubectl -n vamos get $V -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's|containerd://||')"
wait $B; grep -o '"retry 1"' "$TMP/k2.json"; sleep 5; kubectl -n vamos logs $V | grep -i "registered\|cleaning up"

# K3. Ctrl-C kills the running attempt, as R5: expect no attempt cgroup left on any worker.
./vamos build slow --rebuild -C "$TMP/slow" --coordinator http://127.0.0.1:18080 & B=$!
sleep 15; kill -INT $B; wait $B; sleep 5
for p in $(kubectl -n vamos get pods -o name | grep worker); do
  kubectl -n vamos exec $p -- nsenter -t 1 -C -m ls /sys/fs/cgroup/vamos | grep attempt
done
```

Remove the temporary artifacts after the checks: `rm -rf -- "$TMP"`.
