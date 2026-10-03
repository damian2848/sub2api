#!/usr/bin/env bash
# Local pre-release checks scoped to what changed since the last release tag. CI re-runs everything on the
# pushed commit, so this only has to catch the cheap, likely failures before anything is published.
#
#   deploy/releasing/precheck.sh [base-ref]      (default: the newest v* tag)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
base="${1:-$(git describe --tags --abbrev=0 --match 'v*' HEAD 2>/dev/null || git rev-list --max-parents=0 HEAD)}"
changed="$(git diff --name-only "$base"..HEAD)"
echo "checking changes since $base ($(wc -l <<<"$changed" | tr -d ' ') files)"

touches() { grep -Eq "$1" <<<"$changed"; }
failed=0; ran=0
run() { echo "--- $1"; shift; ran=$((ran+1)); if ! "$@"; then echo "FAILED: $*" >&2; failed=1; fi; }

related_frontend_specs() {
  local f dir name stem c
  git diff --name-only "$1"..HEAD -- frontend/src | while read -r f; do
    dir=$(dirname "$f"); name=$(basename "$f"); stem="${name%.*}"
    if [[ "$f" == *.spec.ts || "$f" == *.test.ts ]]; then
      [ -f "$f" ] && echo "$f"
    else
      for c in "$dir/__tests__/$stem.spec.ts" "$dir/__tests__/$stem.test.ts" "$dir/$stem.spec.ts"; do
        [ -f "$c" ] && echo "$c"
      done
    fi
  done | sort -u | sed 's#^frontend/##' | tr '\n' ' '
  return 0
}

export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.0}" GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"
export GOMODCACHE="${GOMODCACHE:-/tmp/sub2api-gomodcache}" GOFLAGS="${GOFLAGS:--mod=mod}" NODE_OPTIONS="${NODE_OPTIONS:---max-old-space-size=8192}"

# Only Go source or module changes need the Go checks; the VERSION file alone does not.
if touches '^backend/.*(\.go|go\.mod|go\.sum)$'; then
  # grep exits 1 on "no match": that must never end the script, so every pipeline below tolerates it.
  pkgs=$( (git diff --name-only "$base"..HEAD -- backend | { grep '\.go$' || true; } | while read -r f; do dirname "$f"; done | sort -u | sed 's#^backend/#./#' | tr '\n' ' ') )
  run "go build" bash -c "cd backend && go build ./..."
  if [ -n "${pkgs// /}" ]; then
    run "go vet + test (changed packages)" bash -c "cd backend && go vet -tags unit $pkgs && go test -tags unit $pkgs -count=1"
  else
    echo "--- go.mod/go.sum changed without a package: running the whole unit suite"
    run "go test (all)" bash -c "cd backend && go test -tags unit ./internal/... -count=1"
  fi
fi
if touches '^frontend/'; then
  run "vue-tsc" bash -c "cd frontend && node_modules/.bin/vue-tsc --noEmit"
  # `vitest --changed` walks the whole module graph and is not reliable in this repo, so the specs are chosen
  # explicitly: every changed spec, plus the specs that sit next to a changed source file. Anything wider is CI's.
  specs=$(related_frontend_specs "$base")
  if [ -n "${specs// /}" ]; then
    # shellcheck disable=SC2086
    run "vitest (changed specs and their neighbours)" bash -c "cd frontend && node_modules/.bin/vitest run $specs"
  else
    echo "--- no frontend spec is related to the change; vue-tsc only (CI runs the full suite)"
  fi
fi
if touches '^tools/prism-browser/'; then
  run "prism-browser unit tests" bash -c "cd tools/prism-browser && node --test test/*.test.mjs"
fi
if touches '^deploy/releasing/'; then
  run "deploy script tests" bash -c "cd deploy/releasing && python3 -m unittest discover -s . -p 'test_deploy_remote.py'"
  run "deploy.sh syntax" bash -n deploy/releasing/deploy.sh
fi
# Generated test files must never ride along into a release commit.
if [ -n "$(git status --porcelain backend/internal/service/data 2>/dev/null)" ]; then
  git checkout -- backend/internal/service/data 2>/dev/null || true; git clean -fdq backend/internal/service/data
  echo "cleaned test artifacts under backend/internal/service/data"
fi
if [ "$ran" = 0 ]; then
  echo "no check matched the changed paths; nothing was verified (docs-only change?)"
  exit 0
fi
[ "$failed" = 0 ] && echo "precheck OK ($ran check(s) ran)" || { echo "precheck FAILED" >&2; exit 1; }
