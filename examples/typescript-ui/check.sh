#!/bin/sh
# Check the tutorial's image and exported website from the repository root.
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
vamos_cli=${VAMOS:-./vamos}
scratch=$(mktemp -d)
container="vamos-typescript-ui-check-$$"
image="$container:check"

cleanup() {
    docker rm -f "$container" >/dev/null 2>&1 || true
    docker image rm "$image" >/dev/null 2>&1 || true
    rm -rf -- "$scratch"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

"$vamos_cli" load frontend "$image" -C "$project_dir"
"$vamos_cli" export build "$scratch/export" -C "$project_dir"
docker run --rm -d --name "$container" "$image" >/dev/null

# Query inside the container: the devcontainer cannot reach the Docker host's localhost.
attempt=0
until docker exec "$container" wget -qO- http://127.0.0.1/ > "$scratch/index.html" 2>/dev/null; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 30 ]; then
        docker logs "$container"
        echo 'Website did not become ready.' >&2
        exit 1
    fi
    sleep 1
done

grep -Fq '<h1>Hello from vamos!</h1>' "$scratch/index.html"
test "$(sha256sum < "$scratch/export/index.html")" = "$(sha256sum < "$scratch/index.html")"

# Fetch the actual bundle referenced by the served page, not the TypeScript source.
bundle=$(sed -n 's/.*<script[^>]*src="\/\([^"]*\.js\)".*/\1/p' "$scratch/index.html")
test -n "$bundle"
docker exec "$container" wget -qO- "http://127.0.0.1/$bundle" > "$scratch/bundle.js"
grep -Fq 'Built with Node.js, served by nginx.' "$scratch/bundle.js"
test "$(sha256sum < "$scratch/export/$bundle")" = "$(sha256sum < "$scratch/bundle.js")"

echo 'TypeScript UI sanity check passed: page, bundle, and exported files agree.'
