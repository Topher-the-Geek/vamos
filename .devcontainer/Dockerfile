FROM fedora:44

# - useful dev tools
# - build dependencies
# - VS Code server dependencies
RUN dnf -y install \
    btrfs-progs-devel \
    buildah \
    containers-common \
    docker-cli \
    docker-compose \
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
    pnpm \
    procps-ng \
    python3 \
    tar \
    tree \
    unzip \
    uv \
    which \
    zip \
 && dnf clean all

# Perfetto's trace processor, to query the traces that `vamos --trace` writes, by SQL.
RUN case "$(uname -m)" in \
      x86_64) arch=amd64 sum=32c739f71b2d39721afd294c0b038f2499a44a71b5b6bcdd85b83faca0b240b9 ;; \
      aarch64) arch=arm64 sum=a82bf4111a340a7ea8577bcfd62e014e8e81b9e6a35a3190f5415fb800051ab0 ;; \
      *) echo "no trace_processor_shell for $(uname -m)" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/perfetto.zip \
      "https://github.com/google/perfetto/releases/download/v58.2/linux-$arch.zip" \
 && echo "$sum  /tmp/perfetto.zip" | sha256sum -c - \
 && unzip -j /tmp/perfetto.zip "linux-$arch/trace_processor_shell" -d /usr/local/bin \
 && chmod 755 /usr/local/bin/trace_processor_shell \
 && rm /tmp/perfetto.zip

 RUN curl -LfsS https://claude.ai/install.sh | bash && \
     curl -LfsS https://chatgpt.com/codex/install.sh | bash && \
     curl -LfsS https://herdr.dev/install.sh | bash

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot

# Container storage must not live on the container's own overlay filesystem.
# Fedora's default storage.conf uses kernel overlayfs, not fuse-overlayfs.
VOLUME /var/lib/containers
