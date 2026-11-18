# syntax=docker/dockerfile:1
# Build context: only flake.nix, flake.lock, nix/, and this Dockerfile.
FROM nixos/nix:2.34.9 AS nix
FROM ubuntu:24.04

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      bash ca-certificates curl openssh-client sudo \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1001 --shell /bin/bash devbox \
    && chmod 0700 /home/devbox \
    && echo 'devbox ALL=(ALL:ALL) NOPASSWD:ALL' > /etc/sudoers.d/devbox \
    && chmod 0440 /etc/sudoers.d/devbox

COPY --from=nix /nix /nix
RUN mkdir -p /etc/nix /workspaces \
    && printf '%s\n' 'experimental-features = nix-command flakes' \
      'build-users-group =' 'sandbox = false' > /etc/nix/nix.conf \
    && chown -R devbox:devbox /nix /workspaces

COPY flake.nix flake.lock /opt/t3fleet-source/
COPY nix/ /opt/t3fleet-source/nix/
ENV HOME=/home/devbox USER=devbox NIX_REMOTE=local
ENV PATH="/home/devbox/.local/state/nix/profiles/t3fleet/bin:/home/devbox/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}"
USER devbox
RUN nix profile add --profile "$HOME/.local/state/nix/profiles/t3fleet" \
      --no-update-lock-file 'path:/opt/t3fleet-source#vm-tools' \
    && t3fleet-image-check \
    && nix store gc

USER root
RUN printf '%s\n' 'export PATH="$HOME/.local/state/nix/profiles/t3fleet/bin:$PATH"' \
      > /etc/profile.d/t3fleet.sh
USER devbox
WORKDIR /workspaces
# The controller invokes t3fleet-runtime-info after every boot. Never start T3 while
# building a reusable image: the first server start creates its unique identity.
CMD ["/bin/bash", "-l"]
