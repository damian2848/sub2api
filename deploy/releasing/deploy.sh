#!/usr/bin/env bash
# Deploy a published release to the production host with the verified rollout (deploy/releasing/deploy_remote.py).
#
#   deploy/releasing/deploy.sh <version> [--execute] [--new-migration NAME.sql=SHA256]... [--sidecar-cmd JSON]
#
# Without --execute it is a read-only preflight. Nothing about the release is edited by hand: the version comes
# from the argument, the commit from the git tag, and the rollback baseline from what is running right now.
set -euo pipefail

HOST="${DEPLOY_HOST:-us-server}"
REMOTE_DIR=/opt/sub2api-gpt56/release-input
SSH=(ssh -o IPQoS=none -o ConnectTimeout=20 -o ServerAliveInterval=30 "$HOST")
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[ $# -ge 1 ] || { sed -n '2,8p' "$0"; exit 2; }
version="$1"; shift
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "version must look like 1.2.3" >&2; exit 2; }
mode=--preflight-only
extra=()
while [ $# -gt 0 ]; do
  case "$1" in
    --execute) mode=--execute ;;
    --new-migration|--sidecar-cmd) extra+=("$1" "$2"); shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

# The commit the tag points at: this is what the operator approved. Fail if the tag is not on the fork yet.
revision="$(git -C "$here" rev-parse --verify --quiet "v${version}^{commit}")" \
  || { echo "tag v${version} does not exist locally" >&2; exit 1; }
git -C "$here" ls-remote --exit-code --tags fork "refs/tags/v${version}" >/dev/null \
  || { echo "tag v${version} is not pushed to the fork" >&2; exit 1; }

# The rollback baseline is whatever is running now, read from the running binary itself.
current="$("${SSH[@]}" 'docker exec gpt56-production-app /app/sub2api -version 2>&1 | grep -o "Sub2API [^ ]* (commit: [0-9a-f]\{40\}"')"
current_version="$(sed -E 's/^Sub2API ([^ ]+) .*/\1/' <<<"$current")"
current_revision="$(sed -E 's/.*commit: ([0-9a-f]{40}).*/\1/' <<<"$current")"
echo "target   v${version} @ ${revision:0:12}"
echo "running  v${current_version} @ ${current_revision:0:12}  (rollback baseline)"
[ "$current_revision" != "$revision" ] || { echo "that release is already running" >&2; exit 1; }

# Stage only the scripts (a few tens of KB). The ~40 MB release archive is downloaded on the host itself.
"${SSH[@]}" "install -d -m 700 ${REMOTE_DIR}/v${version}"
scp -q -o IPQoS=none "$here/deploy_remote.py" "$here/rollout_base.py" "${HOST}:${REMOTE_DIR}/v${version}/"

args=(--version "$version" --revision "$revision" --expect-version "$current_version" --expect-revision "$current_revision" "$mode")
# An empty array is "unbound" under `set -u` in the bash 3.2 that ships with macOS, hence the guard.
if [ "${#extra[@]}" -gt 0 ]; then args+=("${extra[@]}"); fi
if [ "$mode" = --execute ]; then
  # Detached on the host so a dropped connection cannot stop it mid-cutover; progress is read back.
  log="${REMOTE_DIR}/v${version}/rollout.out"; err="${REMOTE_DIR}/v${version}/rollout.err"
  "${SSH[@]}" "cd ${REMOTE_DIR}/v${version} && rm -f rollout.out rollout.err rollout.done && \
    (setsid nohup sh -c 'python3 deploy_remote.py $(printf '%q ' "${args[@]}"); echo \$? > rollout.done' >rollout.out 2>rollout.err </dev/null &) ; echo started"
  while true; do
    sleep 20
    out="$("${SSH[@]}" "cd ${REMOTE_DIR}/v${version} && if [ -f rollout.done ]; then echo DONE \$(cat rollout.done); else echo RUNNING; fi; grep -o '\"stage\": \"[a-z_]*\"' rollout.out | tr '\n' ' '" 2>/dev/null || true)"
    echo "$(date +%H:%M:%S) $(tr '\n' ' ' <<<"$out")"
    grep -q '^DONE' <<<"$out" && break
  done
  "${SSH[@]}" "cd ${REMOTE_DIR}/v${version} && echo '--- stderr:' && cat rollout.err; echo '--- receipt:' && python3 - <<'PY'
import json
t=open('rollout.out').read(); dec=json.JSONDecoder(); i=0
while i<len(t):
    while i<len(t) and t[i].isspace(): i+=1
    if i>=len(t): break
    o,i=dec.raw_decode(t,i)
    if 'verification_passed' in o:
        for k in ('version','revision','app_image','sidecar_image','schema_added','compose_changes','environment_mounts_tuning_preserved','protected_container_ids_preserved','prism_read_only_config_verified','verification_passed','verification_model_requests_made','full_database_restore_performed','backup'): print(k,'=',o.get(k))
PY"
  [ "$("${SSH[@]}" "cat ${REMOTE_DIR}/v${version}/rollout.done")" = 0 ]
else
  "${SSH[@]}" "cd ${REMOTE_DIR}/v${version} && python3 deploy_remote.py $(printf '%q ' "${args[@]}")" | cut -c1-600
fi
