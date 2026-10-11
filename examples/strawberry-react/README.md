# strawberry-react example

A counter web app in two parts:

- `backend/`: a GraphQL API built with [Strawberry](https://strawberry.rocks) on
  [FastAPI](https://fastapi.tiangolo.com). It serves `/graphql`.
- `ui/`: a [React](https://react.dev) page built with [Vite](https://vite.dev) that reads and increments the
  counter with [Apollo Client](https://www.apollographql.com/docs/react).

The UI's TypeScript types come from the backend's schema. The backend exports `schema.graphql` into `ui/`,
and [GraphQL Code Generator](https://the-guild.dev/graphql/codegen) turns it and the queries in
`src/queries.ts` into `src/gql/types.ts`. Both files are generated, so neither is committed.

## Develop

Backend:

```
cd backend
uv venv --python 3.14 .venv
uv pip sync --python .venv requirements_dev_lock.txt
. .venv/bin/activate
pyright && pytest
fastapi dev server.py --port 8100
```

UI, once the backend's virtual environment exists:

```
cd ui
pnpm install
(cd ../backend && .venv/bin/strawberry export-schema schema -o ../ui/schema.graphql)
pnpm codegen
pnpm check           # eslint and tsc
pnpm dev             # http://localhost:3100, proxying /graphql to the backend on port 8100
```

## Build with vamos

The directory is a vamos workspace: its `WORKSPACE` file is empty, and `backend/` and `ui/` each have a `BUILD`
file. The UI's `graphql-codegen` task adds `//backend:schema`, the backend's exported schema, so no generated
file is needed on disk. From the repository root:

```
./vamos load //backend:layer strawberry-backend -C examples/strawberry-react
./vamos load //ui:layer strawberry-ui -C examples/strawberry-react
./vamos build //backend:lint //backend:test //ui:lint -C examples/strawberry-react
```

The checks are tasks without output: building them runs pyright, pytest and eslint.

## Run

The UI's image only serves its static files, and sends GraphQL requests to its own origin's `/graphql`.
In production an HTTP load balancer or an ingress would route that path to the backend. `docker-compose.yaml`
stands in for one with an nginx `ingress` service, which serves both on port 8080. Once both images are loaded:

```
docker compose -f examples/strawberry-react/docker-compose.yaml up
```

Then open http://localhost:8080.

## Dependencies

`backend/requirements.txt` lists what the server needs and `requirements_dev.txt` adds the tools. After
changing either, regenerate both locks, with `--exclude-newer` set to two weeks ago:

```
uv pip compile --python-platform linux --python-version 3.14 --generate-hashes --exclude-newer YYYY-MM-DD \
  requirements.txt -o requirements_lock.txt
uv pip compile --python-platform linux --python-version 3.14 --generate-hashes --exclude-newer YYYY-MM-DD \
  requirements_dev.txt -o requirements_dev_lock.txt
```

`ui/pnpm-workspace.yaml` only lets pnpm install releases at least two weeks old, and saves exact versions.
