#!/usr/bin/env bash
# Run from the development server. No image is pushed unless --publish is given.
set -euo pipefail
cd "$(dirname "$0")/.."

mode=${1:---check}
tag=${2:-}
case "$mode" in
  --check|--build-only) ;;
  --publish)
    if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
      echo 'Usage: bash scripts/publish-server.sh --publish vX.Y.Z' >&2
      exit 2
    fi
    ;;
  *) echo 'Usage: bash scripts/publish-server.sh [--check|--build-only|--publish vX.Y.Z]' >&2; exit 2 ;;
esac

# Serialize release work so two sessions cannot race on moving image tags.
exec 9>/tmp/embyrelay-server-publish.lock
flock -n 9 || { echo 'Another server build/release is running.' >&2; exit 1; }

version=$(sed -n 's/^const Version = "\([^"]*\)"/\1/p' internal/version/version.go)
[[ -n "$version" ]]
revision=$(git rev-parse HEAD)
if [[ "$mode" == --publish ]]; then
  [[ "${tag#v}" == "$version" ]] || { echo 'Tag and source version differ.' >&2; exit 1; }
  [[ -z "$(git status --porcelain --untracked-files=normal)" ]] || { echo 'Commit source changes before publishing.' >&2; exit 1; }
  [[ "$(git rev-parse "$tag^{commit}")" == "$revision" ]] || { echo 'HEAD must match the release tag.' >&2; exit 1; }
  remote_revision=$(git ls-remote origin "refs/tags/$tag^{}" | cut -f1)
  if [[ -z "$remote_revision" ]]; then
    remote_revision=$(git ls-remote origin "refs/tags/$tag" | cut -f1)
  fi
  [[ "$remote_revision" == "$revision" ]] || { echo 'Push the matching tag to GitHub before publishing.' >&2; exit 1; }
  if docker buildx imagetools inspect "ecouus/embyrelay:$version" >/dev/null 2>&1; then
    echo 'This image version already exists; use a new version instead of overwriting.' >&2
    exit 1
  fi
fi

builder=embyrelay-builder
if ! docker buildx inspect "$builder" >/dev/null 2>&1; then
  docker buildx create --name "$builder" --driver docker-container
fi
docker buildx inspect "$builder" --bootstrap >/dev/null
suffix="$$-$(date +%s)"
postgres="embyrelay-release-pg-$suffix"
smoke="embyrelay-release-smoke-$suffix"
image="embyrelay:release-check-$suffix"
cleanup() {
  docker rm -fv "$postgres" "$smoke" >/dev/null 2>&1 || true
  docker image rm "$image" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ "$mode" != --build-only ]]; then
  bash -n scripts/install.sh scripts/install-agent.sh scripts/publish-server.sh
  python3 -m unittest discover -s scripts -p 'test_*.py'
  npm --prefix web ci
  npm --prefix web test
  npm --prefix web run build
  go test -mod=readonly -race ./...
  docker run -d --name "$postgres" -e POSTGRES_PASSWORD=release-test-only \
    -e POSTGRES_DB=embyrelay_test -p 127.0.0.1::5432 postgres:17-alpine >/dev/null
  ready=false
  for _ in $(seq 1 60); do
    if docker exec "$postgres" pg_isready -U postgres -d embyrelay_test >/dev/null 2>&1; then ready=true; break; fi
    sleep 1
  done
  [[ "$ready" == true ]] || { docker logs "$postgres"; exit 1; }
  port=$(docker port "$postgres" 5432/tcp | sed 's/.*://')
  EMBYRELAY_TEST_POSTGRES_URL="postgres://postgres:release-test-only@127.0.0.1:$port/embyrelay_test?sslmode=disable" \
    go test -mod=readonly -race ./internal/control ./internal/database ./migrations
  go vet ./...
  git diff --check
fi

build_args=(--builder "$builder" --platform "linux/amd64,linux/arm64" \
  --progress plain -f deploy/Dockerfile \
  --label "org.opencontainers.image.source=https://github.com/ecouus/EmbyRelay" \
  --label "org.opencontainers.image.revision=$revision" \
  --label "org.opencontainers.image.version=$version")
SECONDS=0
docker buildx build "${build_args[@]}" --load -t "$image" .
echo "Dual-architecture build and local load: ${SECONDS}s"
docker run -d --name "$smoke" -e RELAY_ADMIN_USER=ci \
  -e RELAY_ADMIN_PASSWORD=release-smoke-password -e RELAY_ALLOW_INSECURE=true "$image" >/dev/null
healthy=false
for _ in $(seq 1 30); do
  if docker exec "$smoke" wget -q -O /dev/null http://127.0.0.1:6689/healthz >/dev/null 2>&1; then healthy=true; break; fi
  sleep 1
done
[[ "$healthy" == true ]] || { docker logs "$smoke"; exit 1; }
health=$(docker exec "$smoke" wget -q -O - http://127.0.0.1:6689/healthz)
python3 -c 'import json,sys; d=json.loads(sys.argv[1]); assert d["version"] == sys.argv[2], d; print(d)' "$health" "$version"

if [[ "$mode" == --publish ]]; then
  # Recheck after validation: never publish changes made while tests were running.
  [[ -z "$(git status --porcelain --untracked-files=normal)" && "$(git rev-parse HEAD)" == "$revision" ]]
  tags=(-t "ecouus/embyrelay:$version")
  if [[ "$version" != *-* ]]; then
    tags+=(-t "ecouus/embyrelay:${version%.*}" -t ecouus/embyrelay:latest)
  fi
  SECONDS=0
  docker buildx build "${build_args[@]}" "${tags[@]}" --provenance=true --sbom=true --push .
  echo "Cached build and push: ${SECONDS}s"
  docker buildx imagetools inspect "ecouus/embyrelay:$version"
else
  echo 'Build and startup checks passed. No image pushed.'
fi
