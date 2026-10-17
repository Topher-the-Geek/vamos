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

Remove the temporary artifacts after the checks: `rm -rf -- "$TMP"`.
