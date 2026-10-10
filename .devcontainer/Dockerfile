FROM fedora:44

# - useful dev tools
# - build dependencies
# - VS Code server dependencies
RUN dnf -y install \
    btrfs-progs-devel \
    buildah \
    containers-common \
    docker-cli \
    findutils \
    git \
    golang \
    gpgme-devel \
    gzip \
    jq \
    less \
    netavark \
    nodejs24 \
    nodejs24-npm \
    openssh-clients \
    procps-ng \
    python3 \
    tar \
    tree \
    uv \
    which \
    zip \
 && dnf clean all

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot

# Container storage must not live on the container's own overlay filesystem.
# Fedora's default storage.conf uses kernel overlayfs, not fuse-overlayfs.
VOLUME /var/lib/containers
