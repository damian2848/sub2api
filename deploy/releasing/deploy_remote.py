#!/usr/bin/env python3
"""Private, opt-in, app + Prism release rollout; no model requests.

Run --preflight-only without --execute for read-only production inspection.
See DEPLOYMENT.md. This file and rollout_base.py must remain side by side.
"""
import argparse
import copy
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import time
import urllib.request

import rollout_base as base

BASE, COMPOSE, APP, SIDECAR, VERIFIER = base.BASE, base.COMPOSE, base.APP, base.SIDECAR, base.VERIFIER
# Nothing about one particular release is hard-coded. The target is named by --version/--revision. The
# rollback baseline is the binary running now: it must be healthy and must match the version/revision the
# operator names with --expect-version/--expect-revision, and it is copied out of the container before anything
# changes, so a rollback restores exactly that binary.
VERSION = RELEASE_REVISION = OLD_VERSION = OLD_REVISION = OLD_BINARY_SHA = None
PRISM_DATA = BASE / 'live/prism-browser-data'
STOP_TIMEOUT = 250
# Migrations. The release archive's file list is not trusted for this: the set that must be applied by the new
# binary is passed explicitly (--new-migration NAME=SHA256, repeatable; none for a release without migrations).
# Migrations already applied (the whole current schema) are the baseline and must keep their checksums.
EXPECTED_MIGRATIONS = {}
BASELINE_MIGRATIONS = {}
# Runtime defaults (command, user, ports...) of both services must not change across a release, except for the
# explicit --sidecar-cmd when a release deliberately changes how the sidecar starts.
SIDECAR_CMD_OVERRIDE = None
CODE_PATHS = ['package.json', 'package-lock.json', 'THIRD_PARTY_NOTICES.md', 'src']
CONFIG_FIELDS = {
    'project_isolation': ('PRISM_PROJECT_ISOLATION', False),
    'http_cache': ('PRISM_HTTP_CACHE', False),
    'memory_limit_mib': ('PRISM_MEMORY_LIMIT_MIB', 0),
    'memory_reserve_mib': ('PRISM_MEMORY_RESERVE_MIB', 32),
    'multiplex_pages': ('PRISM_MULTIPLEX_PAGES', False),
    'prewarm_chat': ('PRISM_PREWARM_CHAT', True),
    'stream_reasoning': ('PRISM_STREAM_REASONING', True),
}

# Hash actual container files, not the image label. "all" includes dependencies;
# "code" binds the deployed first-party source to the verified release archive.
NODE_DIGEST = r"""
const fs = require('node:fs'), path = require('node:path'), crypto = require('node:crypto');
const all = process.argv[1] === 'all', files = [];
function walk(p) {
  const s = fs.lstatSync(p), relative = path.relative('/app', p).split(path.sep).join('/');
  if (s.isDirectory()) { if (all) files.push([relative, 'd', p, s]);
    for (const name of fs.readdirSync(p).sort()) walk(path.join(p, name)); }
  else if (s.isFile()) files.push([relative, 'f', p, s]);
  else if (s.isSymbolicLink()) files.push([relative, 'l', p, s]);
  else throw new Error('unsupported_app_filesystem_entry');
}
for (const p of (all ? ['/app'] : ['package.json','package-lock.json','THIRD_PARTY_NOTICES.md','src'].map(p=>path.join('/app',p)))) walk(p);
const h = crypto.createHash('sha256');
for (const [relative, kind, p, s] of files.sort((a,b)=>a[0]<b[0]?-1:a[0]>b[0]?1:0)) {
  h.update(relative+'\0'+kind+'\0');
  if (all) h.update([s.mode & 4095,s.uid,s.gid].join(':')+'\0');
  if (kind==='f') h.update(fs.readFileSync(p));
  else if (kind==='l') h.update(fs.readlinkSync(p));
  h.update('\0');
}
console.log(h.digest('hex'));
"""


class InterruptedRollout(RuntimeError):
    pass


def require(value, message):
    base.require(value, message)


def run(args, **kwargs):
    return base.run(args, **kwargs)


def inspect(name, optional=False):
    return base.inspect(name, optional=optional)


def write(path, data, mode=0o600):
    # Recovery checkpoints must survive process termination between renames.
    path = Path(path)
    temporary = path.with_name(path.name + '.tmp')
    with temporary.open('wb') as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    temporary.chmod(mode)
    temporary.replace(path)
    directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def write_json(path, value):
    write(path, (json.dumps(value, indent=2) + '\n').encode())


def file_sha(path):
    return base.sha(path)


def schema():
    return base.schema()


def sql(query, timeout=180):
    pg = base.environment(json.loads((BASE / 'source-artifacts/containers.json').read_text())['sub2api-postgres'])
    return run(['docker', 'exec', '-i', 'gpt56-postgres-standby', 'psql', '-X', '-q', '-p', '15432',
                '-U', pg['POSTGRES_USER'], '-d', pg['POSTGRES_DB'], '-v', 'ON_ERROR_STOP=1', '-At'],
               data=query.encode(), timeout=timeout).decode().strip()


def protected_snapshot():
    ids = run(['docker', 'ps', '-aq']).decode().split()
    require(ids, 'Docker has no containers')
    containers = json.loads(run(['docker', 'inspect', *ids]))
    return {c['Name'].lstrip('/'): {'id': c['Id'], 'running': c['State']['Running']}
            for c in containers if c['Name'].lstrip('/') not in (APP, SIDECAR)}


def check_protected(expected):
    for name, original in expected.items():
        current = inspect(name)
        require(current['Id'] == original['id'] and current['State']['Running'] == original['running'],
                'An unrelated production container was recreated, removed or stopped')


def prism_digest(image=None, mode='all'):
    args = (['docker', 'run', '--rm', '--network', 'none', '--entrypoint', 'node', image]
            if image else ['docker', 'exec', SIDECAR, 'node'])
    value = run(args + ['-e', NODE_DIGEST, mode], timeout=180).decode().strip()
    require(re.fullmatch(r'[0-9a-f]{64}', value), 'Prism runtime fingerprint is invalid')
    return value


def release_code_digest(source):
    entries = []
    for name in CODE_PATHS:
        path = source / name
        require(path.exists(), 'Prism release source is incomplete')
        paths = [path] if not path.is_dir() else list(path.rglob('*'))
        for item in paths:
            require(not item.is_symlink(), 'Prism release source contains a symlink')
            if item.is_file():
                entries.append(item)
    digest = hashlib.sha256()
    for path in sorted(entries, key=lambda item: item.relative_to(source).as_posix()):
        digest.update((path.relative_to(source).as_posix() + '\0f\0').encode())
        digest.update(path.read_bytes())
        digest.update(b'\0')
    return digest.hexdigest()


def tree_digest(directory):
    """Stopped data tree: content, ownership, mode and symlinks; never follow links."""
    digest = hashlib.sha256()
    paths = []
    def walk(path):
        paths.append(path)
        if path.is_dir() and not path.is_symlink():
            for item in sorted(path.iterdir()):
                walk(item)
    walk(directory)
    for path in sorted(paths, key=lambda p: p.relative_to(directory).as_posix()):
        stat = path.lstat()
        kind = 'l' if path.is_symlink() else 'd' if path.is_dir() else 'f' if path.is_file() else 'unsupported'
        require(kind != 'unsupported', 'Prism data contains a socket/device; resolve it while stopped before rollout')
        digest.update(json.dumps([path.relative_to(directory).as_posix(), kind, stat.st_mode & 0o7777,
                                  stat.st_uid, stat.st_gid], separators=(',', ':')).encode() + b'\0')
        if kind == 'f':
            with path.open('rb') as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                    digest.update(chunk)
        elif kind == 'l':
            digest.update(os.readlink(path).encode())
        digest.update(b'\0')
    return digest.hexdigest()


def checksum_entry(checksums, name):
    matches = []
    for line in checksums.read_text().splitlines():
        match = re.fullmatch(r'([0-9a-fA-F]{64})\s+\*?(.+)', line)
        if match and match.group(2) == name:
            matches.append(match.group(1).lower())
    require(len(matches) == 1, 'Published checksum file must contain exactly one matching archive entry')
    return matches[0]


def verify_archive(path, checksums, expected_name):
    require(path.is_file() and path.name == expected_name, f'Release archive filename does not match v{VERSION}')
    digest = file_sha(path)
    require(digest == checksum_entry(checksums, path.name), 'Published release archive checksum does not match')
    return digest


def safe_extract(archive, target):
    target.mkdir(mode=0o700, parents=True)
    with tarfile.open(archive, 'r:gz') as stream:
        members = stream.getmembers()
        require(len(members) <= 50000 and sum(m.size for m in members) <= 512 * 1024 * 1024,
                'Release archive exceeds extraction bounds')
        names = set()
        for member in members:
            path = PurePosixPath(member.name)
            require(not path.is_absolute() and '..' not in path.parts and member.name not in names
                    and (member.isfile() or member.isdir()), 'Release archive contains an unsafe or duplicate entry')
            names.add(member.name)
            require((target / member.name).resolve().is_relative_to(target.resolve()),
                    'Release archive escapes the staging directory')
        # We have rejected links/devices and traversal; avoid trusting archive owners.
        for member in members:
            path = target / member.name
            if member.isdir():
                path.mkdir(parents=True, exist_ok=True, mode=0o700)
            else:
                path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                with stream.extractfile(member) as source, path.open('xb') as dest:
                    shutil.copyfileobj(source, dest)
                path.chmod(member.mode & 0o777)


def configuration_expected(environment, data_dir):
    saved = data_dir / 'runtime-config.json'
    if saved.exists() or saved.is_symlink():
        require(not saved.is_symlink() and saved.is_file() and saved.stat().st_size <= 4096,
                'Existing saved Prism configuration is unsafe')
        body = json.loads(saved.read_text())
        require(body.get('schema') == 1 and set(body) == {'schema', 'values'}, 'Saved Prism configuration schema changed')
        expected = body['values']
        require(isinstance(expected, dict) and set(expected) == set(CONFIG_FIELDS), 'Saved Prism configuration fields changed')
        origin = 'saved'
    else:
        expected = {}
        for field, (name, default) in CONFIG_FIELDS.items():
            raw = str(environment.get(name, '')).strip().lower()
            if not raw:
                expected[field] = default
            elif isinstance(default, bool):
                require(raw in ('true', 'false', '1', '0', 'on', 'off'), 'Prism boolean tuning is invalid')
                expected[field] = raw in ('true', '1', 'on')
            else:
                # Numeric environment values use JS Number() semantics; production uses integers.
                number = float(raw)
                require(number.is_integer(), 'Prism numeric tuning is not an integer')
                expected[field] = int(number)
        origin = 'environment'
    for field, (_, default) in CONFIG_FIELDS.items():
        value = expected[field]
        require(type(value) is bool if isinstance(default, bool) else
                type(value) is int and 0 <= value <= 1048576, 'Prism saved tuning value is invalid')
    require(expected['memory_limit_mib'] == 0 or expected['memory_limit_mib'] > expected['memory_reserve_mib'],
            'Prism memory limit is smaller than its reserve')
    return {'effective': expected, 'desired': expected, 'restart_required': False, 'source': origin, 'apply_mode': 'restart'}


def preflight():
    state = base.preflight()
    require(state['sidecar'] is not None and state['sidecar']['State']['Running']
            and state['sidecar']['State'].get('Health', {}).get('Status') == 'healthy', 'Existing Prism must be healthy')
    require(state['metadata'] == {'version': OLD_VERSION, 'revision': OLD_REVISION},
            'Production app is not the version/revision the operator expects as the rollback baseline')
    require(not (set(state['schema']) & set(EXPECTED_MIGRATIONS)), 'New migrations are already present; use reviewed recovery instead')
    require(state['config']['services']['prism-browser']['image'] == state['sidecar']['Config']['Image'],
            'Prism Compose image differs from the running service')
    data_mounts = [m for m in state['sidecar']['Mounts'] if m['Destination'] == '/data']
    require(len(data_mounts) == 1 and data_mounts[0]['Type'] == 'bind'
            and data_mounts[0]['Source'] == str(PRISM_DATA) and not PRISM_DATA.is_symlink()
            and PRISM_DATA.is_dir(), 'Prism data mount changed')
    require(not any(m['Destination'].startswith('/data/') for m in state['sidecar']['Mounts']),
            'Nested Prism data mounts require a separate backup plan')
    for service, container in (('app', state['old']), ('prism-browser', state['sidecar'])):
        configured = base.service_environment(state['config']['services'][service])
        require(all(value is not None and base.environment(container).get(name) == str(value)
                    for name, value in configured.items()), 'Compose environment differs from the running service')
        require(not state['config']['services'][service].get('env_file'), 'External environment files require a separate immutable snapshot')
    require(state['sidecar']['HostConfig']['PortBindings'].get('8319/tcp') ==
            [{'HostIp': '127.0.0.1', 'HostPort': '8319'}], 'Prism port is not loopback-only')
    require(base.environment(state['old']).get('PRISM_MANAGEMENT_KEY') ==
            base.environment(state['sidecar']).get('PRISM_MANAGEMENT_KEY'), 'Existing Prism management keys disagree')
    state['protected'] = protected_snapshot()
    state['prism_runtime_sha'] = prism_digest()
    state['compose_mode'] = COMPOSE.stat().st_mode & 0o7777
    return state


def public_preflight(state):
    value = base.public_preflight(state)
    value.update({'prism_runtime_sha256': state['prism_runtime_sha'],
                  'protected_container_count': len(state['protected']), 'rollout_images_only': True})
    return value


def compose_up(services):
    return run(['docker', 'compose', '--project-directory', str(BASE / 'production'), '-f', str(COMPOSE),
                '--profile', 'cutover', 'up', '-d', '--no-deps', '--wait', '--wait-timeout', '300',
                '--timeout', str(STOP_TIMEOUT), *services], merged=True, timeout=700)


def stop_services(backup):
    # App must finish upstream requests before the adapter is stopped.
    for name in (APP, SIDECAR):
        current = inspect(name, optional=True)
        if current and current['State']['Running']:
            write(backup / ('stop-' + name + '.log'),
                  run(['docker', 'stop', '--time', str(STOP_TIMEOUT), name], merged=True, timeout=STOP_TIMEOUT + 90))
        current = inspect(name, optional=True)
        require(current is None or not current['State']['Running'], 'An app/Prism process did not stop')


def raw_facts_fingerprint():
    # Executed only while apps are stopped; fingerprint evidence is private.
    has_ledger = sql("SELECT to_regclass('public.probe_request_facts') IS NOT NULL;") == 't'
    ledger_expr = "(SELECT json_build_object('count',COUNT(*),'max_id',MAX(id),'input',SUM(input_tokens),'output',SUM(output_tokens),'cost',SUM(upstream_cost_usd)) FROM probe_request_facts)" if has_ledger else 'NULL'
    return json.loads(sql(f"""SELECT json_build_object(
      'usage', (SELECT json_build_object('count',COUNT(*),'max_id',MAX(id),
                   'input',SUM(input_tokens),'output',SUM(output_tokens),'cost',SUM(actual_cost)) FROM usage_logs),
      'ops_errors', (SELECT json_build_object('count',COUNT(*),'max_id',MAX(id)) FROM ops_error_logs),
      'probe_facts', {ledger_expr},
      'users', (SELECT md5(COALESCE(string_agg(row_to_json(u)::text,'|' ORDER BY u.id),'')) FROM users u),
      'api_keys', (SELECT md5(COALESCE(string_agg(row_to_json(k)::text,'|' ORDER BY k.id),'')) FROM api_keys k)
    );""", timeout=180))


def restore_prism_data(backup, plan):
    snapshot = backup / 'prism-data.snapshot'
    if not plan.get('prism_snapshot_ready'):
        require(not plan.get('candidate_started'), 'Candidate ran without a stopped Prism data snapshot')
        return
    require(snapshot.is_dir() and not snapshot.is_symlink()
            and tree_digest(snapshot) == plan['prism_data_sha'], 'Stopped Prism data snapshot failed verification')
    journal_path = backup / 'prism-data-restore-journal.json'
    # Recover the exact interruption window between the two directory renames.
    if not PRISM_DATA.exists():
        require(journal_path.is_file() and not journal_path.is_symlink(),
                'Missing active Prism data has no trusted restore journal')
        journal = json.loads(journal_path.read_text())
        replacement, displaced = Path(journal['replacement']), Path(journal['displaced'])
        require(journal.get('target') == str(PRISM_DATA) and journal.get('phase') == 'prepared'
                and journal.get('snapshot_sha256') == plan['prism_data_sha']
                and replacement.parent == PRISM_DATA.parent and displaced.parent == PRISM_DATA.parent
                and replacement.name.startswith('prism-browser-data.restore-')
                and displaced.name.startswith('prism-browser-data.failed-')
                and displaced.is_dir() and not displaced.is_symlink()
                and replacement.is_dir() and not replacement.is_symlink()
                and tree_digest(replacement) == plan['prism_data_sha'],
                'Interrupted Prism data restore journal/paths failed verification')
        replacement.rename(PRISM_DATA)
        journal['phase'] = 'restored'
        write_json(journal_path, journal)
        require(tree_digest(PRISM_DATA) == plan['prism_data_sha'], 'Resumed Prism data restore failed verification')
        return
    require(not PRISM_DATA.is_symlink() and PRISM_DATA.is_dir(), 'Active Prism data path changed')
    suffix = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
    replacement = PRISM_DATA.with_name('prism-browser-data.restore-' + suffix)
    displaced = PRISM_DATA.with_name('prism-browser-data.failed-' + suffix)
    run(['cp', '-a', '--reflink=auto', '--', str(snapshot), str(replacement)], timeout=1200)
    require(tree_digest(replacement) == plan['prism_data_sha'], 'Restored Prism data does not match its snapshot')
    journal = {'phase': 'prepared', 'target': str(PRISM_DATA), 'replacement': str(replacement),
               'displaced': str(displaced), 'snapshot_sha256': plan['prism_data_sha']}
    # fsync checkpoint before changing the active mount source path.
    write_json(journal_path, journal)
    PRISM_DATA.rename(displaced)
    replacement.rename(PRISM_DATA)
    journal['phase'] = 'restored'
    write_json(journal_path, journal)
    write_json(backup / ('prism-data-restore-' + suffix + '.json'),
               {'restored_sha256': plan['prism_data_sha'], 'failed_candidate_data_retained': str(displaced)})


def command_matches(name, now, original):
    """Runtime defaults must be unchanged, except the sidecar must now start the supervisor."""
    for key in ('Cmd', 'Entrypoint', 'User', 'WorkingDir', 'ExposedPorts', 'StopSignal'):
        want = SIDECAR_CMD_OVERRIDE if (name == SIDECAR and key == 'Cmd' and SIDECAR_CMD_OVERRIDE) else original['Config'].get(key)
        if now['Config'].get(key) != want:
            return False
    return True


def verify_migrations(before, after, *, rolled_back=False):
    require(all(after.get(name) == checksum for name, checksum in before.items()),
            'An existing database migration disappeared or changed checksum')
    added = {name: checksum for name, checksum in after.items() if name not in before}
    expected = dict(EXPECTED_MIGRATIONS)
    if rolled_back:
        expected.pop('264_channel_monitor_request_accounting.sql', None)
        # 265 may not have been reached when candidate startup failed.
        require(set(added) <= set(expected) and all(expected.get(name) == checksum for name, checksum in added.items()),
                'Rollback retained an unexpected migration')
    else:
        require(added == expected, 'Candidate applied migrations that this release does not ship')
    return added


def rollback(backup, plan):
    base.BACKUP = backup
    require(plan.get('cutover_started'), 'No cutover was started; recovery must not alter a running service')
    stop_services(backup)
    check_protected(plan['protected'])
    after = schema()
    require(all(after.get(name) == checksum for name, checksum in plan['schema'].items()),
            'Baseline migration checksums changed; automatic rollback is unsafe')
    require(set(after) - set(plan['schema']) <= set(EXPECTED_MIGRATIONS),
            'Unexpected migrations appeared; automatic rollback is unsafe')
    for name in set(after) & set(EXPECTED_MIGRATIONS):
        require(after[name] == EXPECTED_MIGRATIONS[name], 'New migration checksum changed; automatic rollback is unsafe')
    # This script never runs a schema rollback: no release it deploys may ship a destructive migration.
    verify_migrations(plan['schema'], schema(), rolled_back=True)
    restore_prism_data(backup, plan)
    # Only two image references differ: preserve every other original service attribute.
    rollback_config = copy.deepcopy(plan['config'])
    rollback_config['services']['app']['image'] = plan['rollback_app_image']
    rollback_config['services']['prism-browser']['image'] = plan['rollback_prism_image']
    require(inspect(plan['rollback_app_image'])['Id'] == plan['rollback_app_image_id']
            and inspect(plan['rollback_prism_image'])['Id'] == plan['rollback_prism_image_id'], 'Captured rollback images changed')
    write_json(backup / 'compose.rollback.json', rollback_config)
    write(COMPOSE, (backup / 'compose.rollback.json').read_bytes(), plan['compose_mode'])
    write(VERIFIER, (backup / 'verify_target.original.py').read_bytes(), plan['verifier_mode'])
    run(['docker', 'compose', '-f', str(COMPOSE), '--profile', 'cutover', 'config', '-q'])
    write(backup / 'rollback-prism.log', compose_up(['prism-browser']))
    require(prism_digest() == plan['prism_runtime_sha'], 'Restored Prism runtime differs from captured runtime')
    write(backup / 'rollback-app.log', compose_up(['app']))
    current, sidecar = inspect(APP), inspect(SIDECAR)
    for name, now, original in ((APP, current, plan['old']), (SIDECAR, sidecar, plan['sidecar'])):
        require(base.environment(now) == base.environment(original) and now['Mounts'] == original['Mounts'],
                'Rollback environment/mounts differ from original')
        require(now['State'].get('Health', {}).get('Status') == 'healthy', 'Restored app or Prism is unhealthy')
    require(file_sha(f"/proc/{current['State']['Pid']}/exe") == plan['binary_sha']
            and base.binary_metadata(['docker', 'exec', APP, '/app/sub2api']) == plan['metadata'], 'Exact app rollback failed')
    for endpoint in ('/health', '/readyz'):
        base.http(endpoint)
    check_protected(plan['protected'])
    checks = json.loads(run(['python3', str(VERIFIER)], timeout=180))
    write_json(backup / 'rollback-verification.json', checks)
    require(checks.get('version') == OLD_VERSION and checks.get('commit') == OLD_REVISION,
            'Rollback production verification failed')
    receipt = {'rolled_back': True, **plan['metadata'], 'backup': str(backup),
               'exact_runtime_binary_restored': True, 'exact_prism_runtime_restored': True,
               'original_prism_data_snapshot_restored': bool(plan.get('prism_snapshot_ready')),
               'compose_changes_from_original': ['app.image', 'prism-browser.image'],
               'original_image_tags_untouched': True, 'raw_request_and_financial_data_preserved': True,
               'full_database_restore_performed': False, 'monitor_statistics_rebuild_required': True}
    write_json(backup / 'rollback-receipt.json', receipt)
    print(json.dumps(receipt), flush=True)


def rollout(args, state):
    require(args.revision == RELEASE_REVISION, 'Only the named full release revision is accepted')
    paths = {name: Path(getattr(args, name)).resolve() for name in
             ('app_archive', 'checksums', 'prism_archive', 'prism_checksums')}
    app_archive_sha = verify_archive(paths['app_archive'], paths['checksums'], f'sub2api_{VERSION}_linux_amd64.tar.gz')
    prism_archive_sha = verify_archive(paths['prism_archive'], paths['prism_checksums'], f'prism-browser_{VERSION}.tar.gz')
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
    stage = BASE / 'releases' / (f'v{VERSION}-' + args.revision[:12] + '-' + stamp)
    stage.mkdir(mode=0o700, parents=True)
    backup = BASE / 'upgrade-backups' / (f'v{VERSION}-' + stamp)
    backup.mkdir(mode=0o700, parents=True)
    base.BACKUP = backup
    write(backup / 'compose.original.json', state['original'], state['compose_mode'])
    write(backup / 'verify_target.original.py', state['verifier_original'], state['verifier_mode'])
    write_json(backup / 'schema.before.json', state['schema'])
    write_json(backup / 'app.inspect.json', state['old'])
    write_json(backup / 'prism.inspect.json', state['sidecar'])
    write_json(backup / 'protected-containers.json', state['protected'])
    plan = {name: state[name] for name in ('old', 'sidecar', 'metadata', 'schema', 'binary_sha', 'protected',
                                          'prism_runtime_sha', 'config', 'compose_mode', 'verifier_mode')}
    plan.update({'stage': str(stage), 'cutover_started': False, 'candidate_started': False,
                 'prism_snapshot_ready': False, 'release_revision': args.revision})
    write_json(backup / 'recovery.json', plan)
    safe_extract(paths['app_archive'], stage / 'application-archive')
    binary = stage / 'application-archive/sub2api'
    require(binary.is_file() and not binary.is_symlink(), 'Published amd64 archive is missing its main binary')
    binary.chmod(0o755)
    binary_sha = file_sha(binary)
    safe_extract(paths['prism_archive'], stage / 'prism-archive')
    source = stage / f'prism-archive/prism-browser_{VERSION}/tools/prism-browser'
    require(source.is_dir() and (source / 'Dockerfile').is_file(), 'Published Prism package layout changed')
    expected_code_sha = release_code_digest(source)
    base_images = re.findall(r'^FROM\s+(\S+)', (source / 'Dockerfile').read_text(), re.MULTILINE)
    require(len(base_images) == 1 and inspect(base_images[0], optional=True), 'Prism base image is absent; no-pull build cannot proceed')
    # Preserve the original immutable image IDs without moving original tags.
    original_app_image = 'sub2api-gpt56:original-app-layer-' + stamp.lower()
    original_prism_image = 'sub2api-gpt56/prism-browser:original-layer-' + stamp.lower()
    run(['docker', 'tag', state['old']['Image'], original_app_image])
    run(['docker', 'tag', state['sidecar']['Image'], original_prism_image])
    rollback_context = backup / 'rollback-app-image'
    rollback_context.mkdir(mode=0o700)
    run(['docker', 'cp', APP + ':/app/sub2api', str(rollback_context / 'sub2api')])
    require(file_sha(rollback_context / 'sub2api') == state['binary_sha'], 'App runtime changed during capture')
    write(rollback_context / 'Dockerfile', ('FROM ' + original_app_image +
          '\nCOPY --chmod=755 sub2api /app/sub2api\n').encode())
    rollback_app_image = f'sub2api-gpt56:rollback-v{VERSION}-' + stamp.lower()
    write(backup / 'rollback-app-build.log', run(['docker', 'build', '--pull=false', '-t', rollback_app_image,
          str(rollback_context)], merged=True, timeout=600))
    require(base.binary_metadata(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', '/app/sub2api',
                                 rollback_app_image]) == state['metadata'], 'Captured app rollback metadata differs')
    require(run(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', 'sha256sum', rollback_app_image,
                 '/app/sub2api']).decode().split()[0] == state['binary_sha'], 'Captured app rollback digest differs')
    require(prism_digest() == state['prism_runtime_sha'], 'Prism runtime changed before capture')
    rollback_prism_image = f'sub2api-gpt56/prism-browser:rollback-v{VERSION}-' + stamp.lower()
    # /data is a bind mount and is intentionally captured separately only after stop.
    write(backup / 'rollback-prism-capture.log', run(['docker', 'commit', '--pause=false', SIDECAR,
          rollback_prism_image], merged=True, timeout=600))
    require(prism_digest() == state['prism_runtime_sha'] and
            prism_digest(rollback_prism_image) == state['prism_runtime_sha'], 'Captured Prism filesystem differs from runtime')
    app_image = f'sub2api-gpt56:{VERSION}-' + args.revision[:12] + '-' + binary_sha[:12]
    context = stage / 'app-image'
    context.mkdir(mode=0o700)
    shutil.copyfile(binary, context / 'sub2api')
    write(context / 'Dockerfile', (f'FROM {rollback_app_image}\nCOPY --chmod=755 sub2api /app/sub2api\n'
          f'LABEL org.opencontainers.image.version="{VERSION}"\n'
          f'LABEL org.opencontainers.image.revision="{args.revision}"\n').encode())
    write(context / '.dockerignore', b'*\n!Dockerfile\n!sub2api\n')
    write(backup / 'candidate-app-build.log', run(['docker', 'build', '--pull=false', '-t', app_image,
          str(context)], merged=True, timeout=600))
    expected_metadata = {'version': VERSION, 'revision': args.revision}
    require(base.binary_metadata(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', '/app/sub2api',
                                 app_image]) == expected_metadata, 'Published binary full version/revision differs')
    require(run(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', 'sha256sum', app_image,
                 '/app/sub2api']).decode().split()[0] == binary_sha, 'Candidate app image digest differs')
    prism_image = f'sub2api-gpt56/prism-browser:{VERSION}-' + args.revision[:12] + '-' + prism_archive_sha[:12]
    write(backup / 'candidate-prism-build.log', run(['docker', 'build', '--pull=false', '-t', prism_image,
          str(source)], merged=True, timeout=1200))
    require(prism_digest(prism_image, 'code') == expected_code_sha, 'Candidate Prism source differs from the published package')
    candidate_config = copy.deepcopy(state['config'])
    candidate_config['services']['app']['image'] = app_image
    candidate_config['services']['prism-browser']['image'] = prism_image
    normalized = copy.deepcopy(candidate_config)
    normalized['services']['app']['image'] = state['config']['services']['app']['image']
    normalized['services']['prism-browser']['image'] = state['config']['services']['prism-browser']['image']
    require(normalized == state['config'], 'Candidate Compose changes attributes beyond the two image references')
    write_json(backup / 'compose.candidate.json', candidate_config)
    plan.update({'rollback_app_image': rollback_app_image, 'rollback_app_image_id': inspect(rollback_app_image)['Id'],
                 'rollback_prism_image': rollback_prism_image, 'rollback_prism_image_id': inspect(rollback_prism_image)['Id'],
                 'candidate_app_image': app_image, 'candidate_prism_image': prism_image})
    write_json(backup / 'recovery.json', plan)
    backup_output = run(['python3', str(BASE / 'backup_production.py')], timeout=1200)
    write(backup / 'data-backup.json', backup_output)
    full_backup = json.loads(backup_output)
    full_backup_path = Path(full_backup['backup'])
    require(full_backup.get('postgres_archive_verified') and full_backup_path.is_relative_to(BASE / 'backups')
            and (full_backup_path / 'complete.json').is_file(), 'Full production database backup was not verified')
    manifest = json.loads((full_backup_path / 'SHA256.json').read_text())
    require(all(file_sha(full_backup_path / path) == digest for path, digest in manifest.items()),
            'Full production backup manifest failed verification')
    # Reject concurrent hotpatches/config edits or any changed protected container before downtime.
    require(COMPOSE.read_bytes() == state['original'] and inspect(APP)['Id'] == state['old']['Id']
            and inspect(SIDECAR)['Id'] == state['sidecar']['Id'], 'Production deployment changed during preparation')
    require(file_sha(f"/proc/{state['old']['State']['Pid']}/exe") == state['binary_sha']
            and prism_digest() == state['prism_runtime_sha'], 'A production runtime changed during preparation')
    require(schema() == state['schema'], 'Migration metadata changed during preparation')
    data_size = sum((Path(directory) / name).lstat().st_size
                    for directory, _dirs, names in os.walk(PRISM_DATA, followlinks=False)
                    for name in names if (Path(directory) / name).is_file()
                    and not (Path(directory) / name).is_symlink())
    required_free = 2 * data_size + 256 * 1024 * 1024
    require(shutil.disk_usage(PRISM_DATA.parent).free >= required_free and
            shutil.disk_usage(backup).free >= required_free,
            'Insufficient free space for a stopped data snapshot and safe rollback replacement')
    check_protected(state['protected'])
    print(json.dumps({'stage': 'images_and_verified_backup_ready', 'backup': str(backup),
                      'data_backup': full_backup['backup'], 'revision': args.revision}), flush=True)
    try:
        plan['cutover_started'] = True
        write_json(backup / 'recovery.json', plan)
        stop_services(backup)
        snapshot = backup / 'prism-data.snapshot'
        stopped_data_sha = tree_digest(PRISM_DATA)
        run(['cp', '-a', '--reflink=auto', '--', str(PRISM_DATA), str(snapshot)], timeout=1200)
        require(tree_digest(snapshot) == stopped_data_sha and tree_digest(PRISM_DATA) == stopped_data_sha,
                'Stopped Prism snapshot does not match its source')
        plan.update({'prism_snapshot_ready': True, 'prism_data_sha': stopped_data_sha})
        write_json(backup / 'recovery.json', plan)
        expected_configuration = configuration_expected(base.environment(state['sidecar']), snapshot)
        write(VERIFIER, state['verifier_candidate'], state['verifier_mode'])
        write(COMPOSE, (backup / 'compose.candidate.json').read_bytes(), state['compose_mode'])
        run(['docker', 'compose', '-f', str(COMPOSE), '--profile', 'cutover', 'config', '-q'])
        # Set durable recovery flag before either candidate can touch persisted data.
        plan['candidate_started'] = True
        write_json(backup / 'recovery.json', plan)
        write(backup / 'prism-rollout.log', compose_up(['prism-browser']))
        sidecar = inspect(SIDECAR)
        require(prism_digest(mode='code') == expected_code_sha, 'Running Prism source differs from release source')
        key = base.environment(state['sidecar'])['PRISM_MANAGEMENT_KEY']
        request = urllib.request.Request('http://127.0.0.1:8319/internal/config', headers={'Authorization': 'Bearer ' + key})
        with urllib.request.urlopen(request, timeout=30) as response:
            require(response.status == 200, 'Prism read-only configuration endpoint is unavailable')
            prism_configuration = json.load(response)
        require(prism_configuration == expected_configuration, 'Prism startup configuration/tuning changed')
        write_json(backup / 'prism-configuration-verification.json', prism_configuration)
        # The old app is already stopped; no old worker overlaps migration startup.
        write(backup / 'app-rollout.log', compose_up(['app']))
        current = inspect(APP)
        require(current['Image'] == inspect(app_image)['Id'] and sidecar['Image'] == inspect(prism_image)['Id'],
                'Running images differ from the prepared candidate')
        for name, now, original in ((APP, current, state['old']), (SIDECAR, sidecar, state['sidecar'])):
            require(now['State'].get('Health', {}).get('Status') == 'healthy', 'Candidate app or Prism is unhealthy')
            require(base.environment(now) == base.environment(original) and now['Mounts'] == original['Mounts'],
                    'Candidate changed existing environment or mounts')
            require(now['HostConfig'] == original['HostConfig'], 'Candidate changed service resource/network configuration')
            require(command_matches(name, now, original), 'Candidate changed a runtime command/user/port default')
        require(file_sha(f"/proc/{current['State']['Pid']}/exe") == binary_sha and
                base.binary_metadata(['docker', 'exec', APP, '/app/sub2api']) == expected_metadata,
                'Running app version/revision/digest differs from the release artifact')
        for endpoint in ('/health', '/readyz'):
            base.http(endpoint)
        after = schema()
        added = verify_migrations(state['schema'], after)
        write_json(backup / 'schema.after.json', after)
        check_protected(state['protected'])
        checks = json.loads(run(['python3', str(VERIFIER)], timeout=180))
        write_json(backup / 'production-verification.json', checks)
        require(checks.get('version') == VERSION and checks.get('commit') == args.revision,
                'Production HTTP/admin/public verification differs from the release')
        assets = base.frontend()
        logs = run(['docker', 'logs', '--tail', '500', APP], merged=True).decode(errors='replace')
        write(backup / 'candidate-startup.log', logs.encode())
        require(not any(line.startswith('panic:') or re.search(r'"level":"(?:fatal|panic)"|\t(?:FATAL|PANIC|DPANIC)\t', line)
                        for line in logs.splitlines()), 'Candidate emitted a fatal startup error')
        receipt = {'version': VERSION, 'revision': args.revision, 'build_type': 'release', 'artifact_origin': 'published_release',
                   'app_archive_sha256': app_archive_sha, 'binary_sha256': binary_sha,
                   'prism_archive_sha256': prism_archive_sha, 'prism_source_sha256': expected_code_sha,
                   'app_image': app_image, 'app_image_id': current['Image'], 'sidecar_image': prism_image,
                   'sidecar_image_id': sidecar['Image'], 'backup': str(backup), 'data_backup': full_backup['backup'],
                   'schema_added': sorted(added), 'existing_migration_checksums_preserved': True,
                   'compose_changes': ['app.image', 'prism-browser.image'], 'environment_mounts_tuning_preserved': True,
                   'protected_container_count': len(state['protected']), 'protected_container_ids_preserved': True,
                   'prism_read_only_config_verified': True, 'frontend_assets': assets,
                   'verification_passed': True, 'verification_model_requests_made': 0,
                   'scheduled_probes_may_resume': True, 'full_database_restore_performed': False}
        write_json(backup / 'receipt.json', receipt)
        write_json(stage / 'receipt.json', receipt)
        print(json.dumps(receipt, indent=2), flush=True)
    except Exception as error:
        write_json(backup / 'rollout-failure.json', {'error_type': type(error).__name__, 'message': str(error)})
        # Do not silently retry a failed rollback; retain all recovery artifacts and stop.
        try:
            rollback(backup, plan)
        except Exception as recovery_error:
            write_json(backup / 'recovery-failure.json', {'error_type': type(recovery_error).__name__, 'message': str(recovery_error)})
            print(json.dumps({'failed': True, 'rollback_failed': True, 'backup': str(backup),
                              'recovery_requires_review': True}), file=sys.stderr, flush=True)
            raise RuntimeError('Rollout and rollback failed; inspect private recovery diagnostics') from None
        raise RuntimeError('Rollout failed and was rolled back; inspect private diagnostics') from None


RELEASE_REPO = 'damian2848/sub2api'
VERSION_PATTERN = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+')
REVISION_PATTERN = re.compile(r'[0-9a-f]{40}')


def download_release(version, revision, stage):
    """Fetch the public release assets straight onto this host and verify them against the published
    checksums, so the large archive never travels through an operator's machine and the file that is
    verified is the file that is deployed. Returns the four paths the rollout needs."""
    base_url = f'https://github.com/{RELEASE_REPO}/releases/download/v{version}/'
    names = ['checksums.txt', f'sub2api_{version}_linux_amd64.tar.gz', f'prism-browser_{version}.tar.gz',
             f'prism-browser_{version}.tar.gz.sha256']
    stage.mkdir(mode=0o700, parents=True, exist_ok=True)
    paths = {}
    for name in names:
        target = stage / name
        run(['curl', '--fail', '--silent', '--show-error', '--location', '--retry', '3', '--connect-timeout', '15',
             '--max-time', '900', '--output', str(target), base_url + name], timeout=950)
        paths[name] = target
    sums = paths['checksums.txt']
    for name in names[1:3]:
        require(file_sha(paths[name]) == checksum_entry(sums, name), f'Published checksum mismatch: {name}')
    prism_sum = paths[names[3]].read_text().split()
    require(prism_sum == [file_sha(paths[names[2]]), names[2]], 'Prism package checksum file does not match its archive')
    # The tag must point at the revision the operator names, or this is not the release they approved.
    with tarfile.open(paths[names[1]], 'r:gz') as archive:
        member = next((m for m in archive.getmembers() if m.isfile() and PurePosixPath(m.name).name == 'sub2api'), None)
        require(member is not None, 'Release archive has no main binary')
        probe = stage / 'probe-sub2api'
        with archive.extractfile(member) as source, probe.open('wb') as dest:
            shutil.copyfileobj(source, dest)
    probe.chmod(0o700)
    found = re.search(r'Sub2API ([^\s]+) \(commit: ([0-9a-f]{40})', run([str(probe), '-version'], timeout=30, merged=True).decode())
    probe.unlink()
    require(found is not None and found.groups() == (version, revision),
            'The downloaded binary is not the version/revision that was named')
    return paths[names[1]], sums, paths[names[2]], paths[names[3]]


def sidecar_busy():
    """True while the Prism sidecar has a generation in flight: more starts than results in its recent log,
    or workers busy / requests queued. A deploy stops the sidecar, so it waits for a quiet moment."""
    logs = run(['docker', 'logs', '--since', '15m', '-t', SIDECAR], merged=True, timeout=60).decode(errors='replace')
    open_turns = 0
    for line in logs.splitlines():
        match = re.search(r'(\{.*\})\s*$', line)
        if not match:
            continue
        try:
            event = json.loads(match.group(1)).get('event')
        except ValueError:
            continue
        if event == 'upstream_start':
            open_turns += 1
        elif event in ('upstream_result', 'browser_ui_failure'):
            open_turns -= 1
    return open_turns > 0


def wait_for_idle(timeout_seconds, quiet_seconds=20):
    """Wait until the sidecar has been idle for `quiet_seconds` straight, up to `timeout_seconds`. Gives up
    (and refuses to deploy) rather than interrupting a live generation."""
    deadline = time.monotonic() + timeout_seconds
    quiet_since = None
    while True:
        if sidecar_busy():
            quiet_since = None
        else:
            quiet_since = quiet_since or time.monotonic()
            if time.monotonic() - quiet_since >= quiet_seconds:
                return
        require(time.monotonic() < deadline, 'The Prism sidecar did not become idle in time; nothing was changed')
        time.sleep(5)


def main():
    global VERSION, RELEASE_REVISION, OLD_VERSION, OLD_REVISION, OLD_BINARY_SHA
    global EXPECTED_MIGRATIONS, BASELINE_MIGRATIONS, SIDECAR_CMD_OVERRIDE
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--preflight-only', action='store_true')
    parser.add_argument('--execute', action='store_true', help='Required for rollout or recovery; default is never mutate')
    parser.add_argument('--recover', help='Reviewed private upgrade-backup directory to recover after an interrupted cutover')
    parser.add_argument('--version', required=True, help='Release to deploy, e.g. 0.2.24')
    parser.add_argument('--revision', required=True, help='Full 40-hex commit the release tag points at')
    parser.add_argument('--expect-version', required=True, help='Version that must be running now (the rollback baseline)')
    parser.add_argument('--expect-revision', required=True, help='Full commit that must be running now')
    parser.add_argument('--new-migration', action='append', default=[], metavar='NAME=SHA256',
                        help='A migration this release must apply (repeatable); none for a release without migrations')
    parser.add_argument('--sidecar-cmd', help='JSON list; only when this release deliberately changes how the sidecar starts')
    parser.add_argument('--wait-idle', type=int, default=600, metavar='SECONDS',
                        help='Wait up to this long for the sidecar to be idle before cutover (0 = do not wait)')
    # The archives are downloaded and verified on this host unless a verified local copy is supplied.
    parser.add_argument('--app-archive')
    parser.add_argument('--checksums')
    parser.add_argument('--prism-archive')
    parser.add_argument('--prism-checksums')
    args = parser.parse_args()
    os.umask(0o077)
    require(VERSION_PATTERN.fullmatch(args.version) and VERSION_PATTERN.fullmatch(args.expect_version), 'Versions must look like 1.2.3')
    require(REVISION_PATTERN.fullmatch(args.revision) and REVISION_PATTERN.fullmatch(args.expect_revision),
            'Revisions must be full 40-hex commits')
    VERSION, RELEASE_REVISION = args.version, args.revision
    OLD_VERSION, OLD_REVISION = args.expect_version, args.expect_revision
    expected = {}
    for item in args.new_migration:
        name, _, digest = item.partition('=')
        require(re.fullmatch(r'[0-9]+_[A-Za-z0-9_]+\.sql', name) and re.fullmatch(r'[0-9a-f]{64}', digest),
                '--new-migration must be NAME.sql=SHA256')
        expected[name] = digest
    EXPECTED_MIGRATIONS = expected
    if args.sidecar_cmd:
        command = json.loads(args.sidecar_cmd)
        require(isinstance(command, list) and command and all(isinstance(part, str) for part in command), '--sidecar-cmd must be a JSON list of strings')
        SIDECAR_CMD_OVERRIDE = command
    require(args.preflight_only != args.execute, 'Choose --preflight-only or explicitly authorize --execute')
    if args.preflight_only:
        require(not args.recover, 'Recovery is not a read-only preflight')
        OLD_BINARY_SHA = None
        state = preflight()
        print(json.dumps(public_preflight(state)), flush=True)
        return
    require(os.geteuid() == 0, 'Rollout must run as root on the production host')
    # All deployments using this lock are serialized; retain fd until success/rollback.
    with (BASE / '.rollout.lock').open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError('Another rollout holds the production lock') from None
        def interrupted(signum, _frame):
            raise InterruptedRollout('Rollout interrupted by signal ' + str(signum))
        signal.signal(signal.SIGTERM, interrupted)
        signal.signal(signal.SIGINT, interrupted)
        if args.recover:
            backup = Path(args.recover).resolve()
            require(backup.is_relative_to(BASE / 'upgrade-backups') and backup.is_dir()
                    and backup.stat().st_mode & 0o077 == 0, 'Recovery backup path/permissions are unsafe')
            plan = json.loads((backup / 'recovery.json').read_text())
            require(plan.get('release_revision') == RELEASE_REVISION
                    and plan.get('metadata') == {'version': OLD_VERSION, 'revision': OLD_REVISION},
                    'Recovery plan does not match this release/baseline')
            OLD_BINARY_SHA = plan['binary_sha']
            rollback(backup, plan)
            return
        supplied = [args.app_archive, args.checksums, args.prism_archive, args.prism_checksums]
        require(all(supplied) or not any(supplied), 'Give all four archive/checksum paths, or none to download on this host')
        if all(supplied):
            args.app_archive, args.checksums, args.prism_archive, args.prism_checksums = supplied
        else:
            stage = BASE / 'release-input' / f'v{VERSION}'
            app, sums, prism, prism_sum = download_release(VERSION, RELEASE_REVISION, stage)
            args.app_archive, args.checksums, args.prism_archive, args.prism_checksums = map(str, (app, sums, prism, prism_sum))
            print(json.dumps({'stage': 'release_downloaded_and_verified', 'version': VERSION}), flush=True)
        state = preflight()
        print(json.dumps(public_preflight(state)), flush=True)
        if args.wait_idle > 0:
            wait_for_idle(args.wait_idle)
            print(json.dumps({'stage': 'sidecar_idle'}), flush=True)
        rollout(args, state)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'failed': True, 'error_type': type(error).__name__, 'message': str(error)}), file=sys.stderr)
        raise SystemExit(1)
