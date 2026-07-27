#!/usr/bin/env bash
#
# make docker-build — the one container image every role runs from: `serve` (a
# head) and `work` (a follower) are the same binary with different arguments.
#
# Version metadata mirrors scripts/build.sh's ldflags, but arrives as build args
# because .dockerignore keeps .git out of the build context (see Dockerfile).
#
# The platform is derived from the host rather than left to Docker's default. The
# image needs cgo — uber/h3-go wraps the C H3 library — and k3d runs k3s nodes at
# the host's architecture, so a native build is both correct and the only fast
# one. A globally exported DOCKER_DEFAULT_PLATFORM (a common workaround for other
# tooling) would otherwise produce an image that runs under emulation at a crawl,
# which is why this passes --platform explicitly and warns when the environment
# disagrees.
#
# `make demo` calls this for you; the target exists so you can build the image
# without standing a cluster up.
#
# Overridable via environment: IMAGE, CONTAINER_RUNNER, PLATFORM.
set -euo pipefail

IMAGE="${IMAGE:-beeline:demo}"
CONTAINER_RUNNER="${CONTAINER_RUNNER:-docker}"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

case "$(uname -m)" in
  arm64 | aarch64) HOST_PLATFORM="linux/arm64" ;;
  *) HOST_PLATFORM="linux/amd64" ;;
esac
PLATFORM="${PLATFORM:-${HOST_PLATFORM}}"

if [[ -n "${DOCKER_DEFAULT_PLATFORM:-}" && "${DOCKER_DEFAULT_PLATFORM}" != "${PLATFORM}" ]]; then
  echo "docker_build: DOCKER_DEFAULT_PLATFORM=${DOCKER_DEFAULT_PLATFORM} disagrees with this host (${PLATFORM});" >&2
  echo "              building ${PLATFORM} anyway — set PLATFORM=... to override deliberately" >&2
fi

# Fallbacks for a shallow clone or a checkout without git, matching build.sh.
COMMIT_HASH="$(git -C "${PROJECT_ROOT}" rev-parse HEAD 2>/dev/null || echo unknown)"
COMMIT_TIME="$(git -C "${PROJECT_ROOT}" log -1 --format=%cI HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u -Iseconds 2>/dev/null || echo unknown)"

echo "▶ building ${IMAGE} for ${PLATFORM}"
echo "  a cold build compiles the cgo H3 bindings — expect a few minutes the first time"

"${CONTAINER_RUNNER}" build \
  --platform "${PLATFORM}" \
  --tag "${IMAGE}" \
  --build-arg "COMMIT_HASH=${COMMIT_HASH}" \
  --build-arg "BUILD_TIME=${BUILD_TIME}" \
  --build-arg "COMMIT_TIME=${COMMIT_TIME}" \
  --file "${PROJECT_ROOT}/Dockerfile" \
  "${PROJECT_ROOT}"
