# Devcontainers copy the binary from this image,
# and Debian 12's glibc is old enough for it to run on Ubuntu 24.04 and later.
# Buildah's namespace setup needs cgo, so the binary can't be static;
# these tags drop its other C libraries: signatures are checked in Go, not by gpgme, and there is no btrfs storage.
FROM golang:1.26-bookworm AS build

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal/ ./internal/
RUN go build -tags "containers_image_openpgp exclude_graphdriver_btrfs" -o vamos .

FROM fedora:44

RUN dnf -y install \
    containers-common \
    netavark \
    && dnf clean all

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot

# Container storage must not live on the container's own overlay filesystem.
VOLUME /var/lib/containers

WORKDIR /build
COPY --from=build /build/vamos /usr/local/bin/vamos

ENTRYPOINT ["vamos"]
