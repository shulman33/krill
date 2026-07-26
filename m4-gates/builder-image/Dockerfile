# The builder VM's rootfs: the toolchain that runs someone else's Dockerfile
# so the host never has to.
#
# This image is TRUSTED input, built once per host by the operator from this
# directory — exactly like the guest kernel. Nothing a deploying user submits
# ever changes it; their context arrives on a separate read-only disk.
#
# BuildKit rather than dockerd, and the `native` snapshotter rather than
# overlayfs: a microVM kernel cannot be assumed to carry overlayfs, and a
# builder that only works on some kernels is a builder that fails on the day
# someone changes the kernel. Native copies instead of layering — slower, and
# it works everywhere.
FROM debian:bookworm-slim

ARG BUILDKIT_VERSION=0.17.2
ARG TARGETARCH=amd64

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl e2fsprogs iproute2 jq tar xz-utils \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL "https://github.com/moby/buildkit/releases/download/v${BUILDKIT_VERSION}/buildkit-v${BUILDKIT_VERSION}.linux-${TARGETARCH}.tar.gz" \
      | tar -xz -C /usr/local --strip-components=1 \
    && buildctl --version && buildkitd --version

COPY krill-build.sh /krill-build.sh
RUN chmod 0755 /krill-build.sh

# Never actually used as a container — the ext4 image this produces is booted
# directly with init=/krill-build.sh.
CMD ["/krill-build.sh"]
