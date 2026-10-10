# vamos

A Go program that uses the [buildah](https://github.com/containers/buildah) library to build
a container image (`docker.io/myusername/my-image`) from `node:12-alpine`, adding `script.js`
and running a build step inside it. The image is written to `/output/my-image.tar` as a
`docker-archive` tarball.

## Build

```
docker build . -t buildah-test
```

## Run

Buildah needs to create namespaces and mounts, which Docker's defaults don't allow.
Without extra permissions the run fails with
`Error during unshare(CLONE_NEWUSER): Operation not permitted`.

Run with full privileges:

```
docker run --rm -it --privileged -v "$PWD/output:/output" buildah-test
```

Or grant only what buildah needs:

```
docker run --rm -it \
  --cap-add SYS_ADMIN \
  --security-opt seccomp=unconfined \
  --security-opt apparmor=unconfined \
  --security-opt label=disable \
  -v "$PWD/output:/output" \
  buildah-test
```

On success it prints `Image built! <image-id>`.

## Run the built image

```
docker load -i output/my-image.tar
docker run --rm myusername/my-image
```

This runs `script.js`, which prints the time the image was built.
