#!/usr/bin/env bash
set -euo pipefail
: "${VERSION:?set VERSION}"
: "${ARCH:?set ARCH}"
[[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]
[[ $ARCH == amd64 || $ARCH == arm64 ]]
runtime_output=$(mktemp -d)
trap 'rm -rf "$runtime_output"' EXIT
cache_args=()
if [[ -n ${REAUTH_CACHE_DIR:-} ]]; then
  if [[ -f "$REAUTH_CACHE_DIR/index.json" ]]; then
    cache_args+=(--cache-from "type=local,src=$REAUTH_CACHE_DIR")
  fi
  cache_args+=(--cache-to "type=local,dest=$runtime_output/cache,mode=max")
fi
docker buildx build --platform "linux/$ARCH" -f tools/reauth-runtime/Dockerfile \
  "${cache_args[@]}" --output "type=local,dest=$runtime_output/artifact" .
mkdir -p reauth-output
tar --dereference -czf "reauth-output/sub2api-reauth_${VERSION}_linux_${ARCH}.tar.gz" -C "$runtime_output/artifact" .
sha256sum "reauth-output/sub2api-reauth_${VERSION}_linux_${ARCH}.tar.gz"
if [[ -n ${REAUTH_CACHE_DIR:-} ]]; then
  mkdir -p "$REAUTH_CACHE_DIR"
  # BuildKit otherwise retains obsolete blobs indefinitely.
  rm -rf "${REAUTH_CACHE_DIR:?}"
  mv "$runtime_output/cache" "$REAUTH_CACHE_DIR"
fi
