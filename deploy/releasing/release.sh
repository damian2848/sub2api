#!/usr/bin/env bash
# Publish a release: push main, tag it and wait for CI and the Release workflow IN PARALLEL, then verify the
# release page. No separate dry run: the tag-triggered Release builds exactly what a dry run would.
#
#   deploy/releasing/release.sh <version> <notes.md>
#
# Preconditions it enforces: clean tree, backend/cmd/server/VERSION == <version>, notes start with
# "Sub2API <version>", the tag does not exist yet, and the fork (never origin) is the push target.
# If CI fails after the tag was pushed the release is already public: it says so, loudly; delete the
# release and tag, fix, and publish the next patch number (never reuse a number).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
REPO="${RELEASE_REPO:-damian2848/sub2api}"; REMOTE=fork
here="$(git rev-parse --show-toplevel)/deploy/releasing"

[ $# -eq 2 ] || { sed -n '2,12p' "$0"; exit 2; }
version="$1"; notes="$2"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "version must look like 1.2.3" >&2; exit 2; }
[ -f "$notes" ] || { echo "notes file not found: $notes" >&2; exit 2; }
tag="v$version"

[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "working tree has uncommitted changes" >&2; exit 1; }
[ "$(cat backend/cmd/server/VERSION)" = "$version" ] || { echo "backend/cmd/server/VERSION is not $version" >&2; exit 1; }
head -1 "$notes" | grep -qx "Sub2API $version" || { echo "notes must start with 'Sub2API $version'" >&2; exit 1; }
[ "$(grep -c '^## ' "$notes")" -ge 4 ] || { echo "notes are missing their ## sections" >&2; exit 1; }
git rev-parse -q --verify "refs/tags/$tag" >/dev/null && { echo "tag $tag already exists" >&2; exit 1; }
[ "$(git branch --show-current)" = main ] || { echo "release from main" >&2; exit 1; }
sha="$(git rev-parse HEAD)"

echo "pushing main ($sha) and tag $tag to $REMOTE"
git push "$REMOTE" main
git tag -a "$tag" -F "$notes" --cleanup=verbatim "$sha"     # --cleanup=verbatim keeps the '## ' headings
git push "$REMOTE" "$tag"

# Both workflows start on the push. Wait for them together and report each as it finishes.
ci_id=""; rel_id=""
for _ in $(seq 1 30); do
  ci_id="${ci_id:-$(gh run list --repo "$REPO" --limit 30 --json databaseId,name,event,headSha --jq "[.[]|select(.name==\"CI\" and .headSha==\"$sha\")][0].databaseId // empty")}"
  rel_id="${rel_id:-$(gh run list --repo "$REPO" --limit 30 --json databaseId,name,event,headSha,headBranch --jq "[.[]|select(.name==\"Release\" and .headSha==\"$sha\" and .headBranch==\"$tag\")][0].databaseId // empty")}"
  [ -n "$ci_id" ] && [ -n "$rel_id" ] && break; sleep 5
done
[ -n "$rel_id" ] || { echo "the Release workflow did not start for $tag" >&2; exit 1; }
echo "CI run: ${ci_id:-none}   Release run: $rel_id"

state() { gh run view "$1" --repo "$REPO" --json status,conclusion --jq '.status + "/" + (.conclusion // "-")'; }
ci_done=""; rel_done=""
[ -z "$ci_id" ] && ci_done="none"
while [ -z "$ci_done" ] || [ -z "$rel_done" ]; do
  [ -z "$ci_done" ] && { s="$(state "$ci_id")"; case "$s" in completed/*) ci_done="$s"; echo "$(date +%H:%M:%S) CI      $s";; esac; }
  [ -z "$rel_done" ] && { s="$(state "$rel_id")"; case "$s" in completed/*) rel_done="$s"; echo "$(date +%H:%M:%S) Release $s";; esac; }
  { [ -z "$ci_done" ] || [ -z "$rel_done" ]; } && sleep 20
done

problems=0
[ "$rel_done" = "completed/success" ] || { echo "RELEASE WORKFLOW FAILED: $rel_done" >&2; problems=1; }
case "$ci_done" in completed/success|none) ;; *) echo "CI FAILED on the published commit: $ci_done  (the release page is already public)" >&2; problems=1;; esac
[ "$problems" = 0 ] || exit 1

# The published release must be what the workflow promised: every platform archive, re-login runtime and
# optional Prism source package, with headings intact and not a draft. Keep this list in one tested helper so
# adding an asset to release.yml cannot leave this post-publish check stale.
gh release view "$tag" --repo "$REPO" --json assets,isDraft,body \
  --jq '{draft:.isDraft, assets:(.assets|length), headings:(.body|test("## ")), names:[.assets[].name]}'
asset_names="$(gh release view "$tag" --repo "$REPO" --json assets --jq '.assets[].name')"
python3 "$here/release_assets.py" "$version" <<<"$asset_names"
echo "released $tag ($sha)"
