#!/bin/bash
# Fixed libc build baseline; the CI job itself stays on ubuntu-latest.
set -euo pipefail
source "$(dirname "$0")/common.sh"
[[ "$#" == 3 && "$1" == /* ]]
need docker 'Docker is required for the hosted Linux release build.'
output="$1"
version="$2"
revision="$3"
go_root="$(go env GOROOT)"
module_cache="$(go env GOMODCACHE)"
builder="$(mktemp -d /tmp/data-mate-builder.XXXXXX)"
image="data-mate-builder:$(id -u)-$$"
cleanup() {
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$builder"
}
trap cleanup EXIT
cat >"$builder/Dockerfile" <<'DOCKERFILE'
FROM ubuntu:24.04@sha256:008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3
RUN apt-get update && apt-get install -y --no-install-recommends build-essential ca-certificates && rm -rf /var/lib/apt/lists/*
DOCKERFILE
docker build --platform linux/amd64 --tag "$image" "$builder"
docker run --rm --platform linux/amd64 --network none --user "$(id -u):$(id -g)" \
  -v "$project_dir:/src:ro" -v "$go_root:/opt/go:ro" -v "$module_cache:/gomod:ro" \
  -v "$(dirname "$output"):/output" -w /src \
  -e PATH=/opt/go/bin:/usr/bin:/bin -e GOTOOLCHAIN=local -e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=amd64 \
  -e GOPROXY=off -e GOSUMDB=off -e GOMODCACHE=/gomod -e GOCACHE=/tmp/go-cache \
  "$image" go build -mod=readonly -trimpath \
  -ldflags "-X main.version=$version -X main.revision=$revision -X main.dirty=false -X main.environment=production" \
  -o "/output/$(basename "$output")" ./cmd/data-mate
