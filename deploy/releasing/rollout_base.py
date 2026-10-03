"""Prepare exact rollback images and deploy the private Prism browser adapter.

Run --preflight-only through SSH stdin for a read-only inspection. A rollout
requires --binary, --binary-sha and --source (the tools/prism-browser directory).
No OAuth account is created and no model request is made by this script.
"""

import argparse
import ast
import copy
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import urllib.request


BASE = Path('/opt/sub2api-gpt56')
COMPOSE = BASE / 'production/compose.json'
APP = 'gpt56-production-app'
SIDECAR = 'gpt56-production-prism-browser'
VERIFIER = BASE / 'verify_target.py'
OTHERS = ('gpt56-postgres-standby', 'gpt56-redis-standby',
          'gpt56-production-grok-video', 'gpt56-production-video-console')
PRISM_ENV = {'PRISM_BROWSER_ENABLED', 'PRISM_BROWSER_BASE_URL', 'PRISM_MANAGEMENT_KEY'}
BACKUP = None


def run(args, *, data=None, timeout=120, merged=False):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT if merged else subprocess.PIPE,
                            timeout=timeout)
    if result.returncode:
        if BACKUP is not None:
            (BACKUP / 'failure.log').write_bytes(result.stdout + (result.stderr or b''))
        raise RuntimeError(f'Command failed: {args[:3]}; private diagnostics are retained in the upgrade backup')
    return result.stdout


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def inspect(name, optional=False):
    if optional:
        result = subprocess.run(['docker', 'inspect', name], stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=30)
        if result.returncode:
            return None
        return json.loads(result.stdout)[0]
    return json.loads(run(['docker', 'inspect', name]))[0]


def environment(container):
    return dict(value.split('=', 1) for value in container['Config']['Env'] if '=' in value)


def binary_metadata(args):
    text = run(args + ['-version'], merged=True).decode()
    match = re.search(r'Sub2API ([0-9A-Za-z.+_-]+) \(commit: ([^\s,]+)', text)
    require(match is not None, 'Binary release metadata is missing')
    return {'version': match.group(1), 'revision': match.group(2)}


def sha(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def write_private(path, data, mode=0o600):
    path = Path(path)
    temporary = path.with_name(path.name + '.tmp')
    with temporary.open('wb') as stream:
        stream.write(data)
    temporary.chmod(mode)
    temporary.replace(path)


def write_json(path, value):
    write_private(path, (json.dumps(value, indent=2) + '\n').encode())


def schema():
    metadata = json.loads((BASE / 'source-artifacts/containers.json').read_bytes())
    pg = environment(metadata['sub2api-postgres'])
    query = 'SELECT filename,checksum FROM schema_migrations ORDER BY filename;'
    data = run(['docker', 'exec', '-i', 'gpt56-postgres-standby', 'psql', '-X', '-q',
                '-p', '15432', '-U', pg['POSTGRES_USER'], '-d', pg['POSTGRES_DB'],
                '-v', 'ON_ERROR_STOP=1', '-At', '-F', '\t'], data=query.encode())
    return dict(line.split('\t', 1) for line in data.decode().strip().splitlines())


def http(path, port=18080):
    with urllib.request.urlopen(f'http://127.0.0.1:{port}' + path, timeout=30) as response:
        require(response.status == 200, 'Local health or asset endpoint failed')
        return response.headers.get('Content-Type', ''), response.read()


def frontend():
    content_type, body = http('/')
    require('text/html' in content_type and b'<div id="app"' in body, 'Frontend HTML is unavailable')
    assets = sorted(set(re.findall(r'(?:src|href)="(/assets/[^\"]+\.(?:js|css))"', body.decode())))
    require(bool(assets), 'Frontend entry assets are missing')
    result = {}
    for asset in assets:
        _, content = http(asset)
        require(bool(content), 'Frontend entry asset is empty')
        result[asset] = hashlib.sha256(content).hexdigest()
    return result


def compatible_verifier(source):
    # Change only the parsed regex literal; the monitor also runs on source builds.
    old = r'Sub2API (\d+\.\d+\.\d+) \(commit: ([0-9a-f]{40})'
    new = r'Sub2API ([0-9A-Za-z.+_-]+) \(commit: ([0-9a-f]{40})'
    tree = ast.parse(source)
    constants = [node for node in ast.walk(tree) if isinstance(node, ast.Constant)]
    if any(node.value == new for node in constants):
        return source
    matches = [node for node in constants if node.value == old]
    require(len(matches) == 1, 'Production verifier metadata parser requires manual review')
    node = matches[0]
    lines = source.splitlines(keepends=True)
    begin = sum(len(line) for line in lines[:node.lineno - 1]) + node.col_offset
    end = sum(len(line) for line in lines[:node.end_lineno - 1]) + node.end_col_offset
    replacement = repr(new).encode()
    changed = source[:begin] + replacement + source[end:]
    ast.parse(changed)
    return changed


def preflight():
    old = inspect(APP)
    require(old['State']['Running'] and old['State'].get('Health', {}).get('Status') == 'healthy',
            'Production app is not healthy')
    require(old['HostConfig']['NetworkMode'] == 'host', 'Production network mode changed')
    original = COMPOSE.read_bytes()
    config = json.loads(original)
    require(config['services']['app']['container_name'] == APP, 'Production app container identity changed')
    require(config['services']['app']['image'] == old['Config']['Image'], 'Compose app image does not match the running app')
    sidecar = inspect(SIDECAR, optional=True)
    require(('prism-browser' in config['services']) == (sidecar is not None),
            'Prism container and Compose ownership are inconsistent')
    if sidecar is None:
        listeners = run(['ss', '-H', '-ltn', 'sport', '=', ':8319'])
        require(not listeners.strip(), 'Port 8319 is already occupied')
    else:
        require(config['services']['prism-browser']['container_name'] == SIDECAR,
                'Prism service container identity changed')
    others = {name: inspect(name)['Id'] for name in OTHERS}
    for name in OTHERS:
        require(inspect(name)['State']['Running'], 'A preserved production dependency is stopped')
    for endpoint in ('/health', '/readyz'):
        http(endpoint)
    metadata = binary_metadata(['docker', 'exec', APP, '/app/sub2api'])
    verifier_original = VERIFIER.read_bytes()
    verifier_candidate = compatible_verifier(verifier_original)
    return {'old': old, 'original': original, 'config': config, 'sidecar': sidecar,
            'others': others, 'metadata': metadata, 'schema': schema(),
            'binary_sha': sha(f"/proc/{old['State']['Pid']}/exe"),
            'verifier_original': verifier_original, 'verifier_candidate': verifier_candidate,
            'verifier_mode': VERIFIER.stat().st_mode & 0o7777}


def public_preflight(state):
    return {'stage': 'preflight', 'read_only': True, **state['metadata'],
            'app_image': state['old']['Config']['Image'], 'app_container_id': state['old']['Id'],
            'app_binary_sha256': state['binary_sha'], 'schema_migration_count': len(state['schema']),
            'sidecar_present': state['sidecar'] is not None, 'preserved_containers': list(state['others']),
            'compose_path': str(COMPOSE), 'network': 'host',
            'verifier_requires_metadata_patch': state['verifier_candidate'] != state['verifier_original']}


def source_digest(source):
    digest = hashlib.sha256()
    for path in sorted(source.rglob('*')):
        relative = path.relative_to(source)
        if not path.is_file() or any(part in ('.git', 'node_modules') for part in relative.parts):
            continue
        digest.update(str(relative).encode() + b'\x00')
        digest.update(path.read_bytes())
    return digest.hexdigest()


def management_key(state):
    directory = BASE / 'secrets'
    directory.mkdir(mode=0o700, exist_ok=True)
    directory.chmod(0o700)
    path = directory / 'prism-browser-management-key'
    if path.exists():
        require(not path.is_symlink() and path.stat().st_mode & 0o077 == 0,
                'Prism management key file permissions are unsafe')
        key = path.read_text().strip()
    else:
        key = environment(state['sidecar']).get('PRISM_MANAGEMENT_KEY') if state['sidecar'] else None
        key = key or secrets.token_urlsafe(48)
        write_private(path, (key + '\n').encode())
    require(32 <= len(key) <= 256 and '\n' not in key and '\r' not in key,
            'Prism management key is invalid')
    if state['sidecar']:
        require(environment(state['sidecar']).get('PRISM_MANAGEMENT_KEY') == key,
                'Existing Prism management key does not match its private file')
    return key


def service_environment(service):
    current = service.get('environment') or {}
    if isinstance(current, dict):
        return dict(current)
    return dict(value.split('=', 1) for value in current)


def rollout(args, state):
    global BACKUP
    binary, source = Path(args.binary).resolve(), Path(args.source).resolve()
    require(binary.is_file() and sha(binary) == args.binary_sha, 'Candidate binary checksum does not match')
    require(source.is_dir() and (source / 'Dockerfile').is_file() and (source / 'package-lock.json').is_file(),
            'Prism browser build source is incomplete')
    require(re.fullmatch(r'[0-9A-Za-z][0-9A-Za-z.+_-]{0,79}', args.version), 'Candidate version is invalid')
    bases = re.findall(r'^FROM\s+(\S+)', (source / 'Dockerfile').read_text(), re.MULTILINE)
    require(len(bases) == 1 and inspect(bases[0], optional=True) is not None,
            'Prism browser base image must already be present before a no-pull build')
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    stage = BASE / 'releases' / f'{args.version}-{args.binary_sha[:12]}'
    stage.mkdir(mode=0o700, parents=True, exist_ok=True)
    BACKUP = BASE / 'upgrade-backups' / f'{args.version}-{stamp}'
    BACKUP.mkdir(mode=0o700)
    write_private(BACKUP / 'compose.original.json', state['original'])
    write_private(BACKUP / 'verify_target.original.py', state['verifier_original'])
    write_json(BACKUP / 'app.inspect.json', state['old'])
    write_json(BACKUP / 'schema.before.json', state['schema'])
    if state['sidecar']:
        write_json(BACKUP / 'prism.inspect.json', state['sidecar'])

    rollback_context = BACKUP / 'rollback-image'
    rollback_context.mkdir(mode=0o700)
    run(['docker', 'cp', APP + ':/app/sub2api', str(rollback_context / 'sub2api')])
    require(sha(rollback_context / 'sub2api') == state['binary_sha'], 'Current binary changed since preflight')
    base_image, rollback_image = f'sub2api-gpt56:runtime-base-{stamp}', f'sub2api-gpt56:rollback-{stamp}'
    run(['docker', 'tag', state['old']['Image'], base_image])
    write_private(rollback_context / 'Dockerfile',
                  f'FROM {base_image}\nCOPY --chmod=755 sub2api /app/sub2api\n'.encode())
    write_private(BACKUP / 'rollback-build.log', run(['docker', 'build', '--pull=false', '-t', rollback_image,
                  str(rollback_context)], timeout=300, merged=True))
    require(binary_metadata(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', '/app/sub2api', rollback_image]) == state['metadata'],
            'Rollback binary metadata does not match the current app')
    require(run(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', 'sha256sum', rollback_image,
                 '/app/sub2api']).decode().split()[0] == state['binary_sha'], 'Rollback binary checksum does not match')
    rollback_config = copy.deepcopy(state['config'])
    rollback_config['services']['app']['image'] = rollback_image
    if state['sidecar']:
        rollback_sidecar = f'sub2api-gpt56/prism-browser:rollback-{stamp}'
        run(['docker', 'tag', state['sidecar']['Image'], rollback_sidecar])
        rollback_config['services']['prism-browser']['image'] = rollback_sidecar
    write_json(BACKUP / 'compose.rollback.json', rollback_config)

    candidate_context = stage / 'app-image'
    candidate_context.mkdir(mode=0o700, exist_ok=True)
    shutil.copyfile(binary, candidate_context / 'sub2api')
    (candidate_context / 'sub2api').chmod(0o755)
    app_image = f'sub2api-gpt56:{args.version}-{args.binary_sha[:12]}'
    write_private(candidate_context / 'Dockerfile',
                  f'FROM {rollback_image}\nCOPY --chmod=755 sub2api /app/sub2api\n'
                  f'LABEL org.opencontainers.image.version="{args.version}"\n'.encode())
    write_private(candidate_context / '.dockerignore', b'*\n!Dockerfile\n!sub2api\n')
    write_private(BACKUP / 'candidate-build.log', run(['docker', 'build', '--pull=false', '-t', app_image,
                  str(candidate_context)], timeout=300, merged=True))
    candidate_metadata = binary_metadata(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', '/app/sub2api', app_image])
    require(candidate_metadata['version'] == args.version, 'Candidate binary version does not match the requested source build')
    require(re.fullmatch(r'[0-9a-f]{40}', candidate_metadata['revision']), 'Candidate commit metadata is required by the production verifier')
    require(run(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', 'sha256sum', app_image,
                 '/app/sub2api']).decode().split()[0] == args.binary_sha, 'Built candidate binary checksum does not match')
    sidecar_source_sha = source_digest(source)
    sidecar_image = f'sub2api-gpt56/prism-browser:{args.version}-{sidecar_source_sha[:12]}'
    write_private(BACKUP / 'prism-build.log', run(['docker', 'build', '--pull=false', '-t', sidecar_image,
                  str(source)], timeout=900, merged=True))
    key = management_key(state)
    data_dir = BASE / 'live/prism-browser-data'
    data_dir.mkdir(mode=0o700, exist_ok=True)
    data_dir.chmod(0o700)
    config = copy.deepcopy(state['config'])
    config['services']['app']['image'] = app_image
    app_environment = service_environment(config['services']['app'])
    app_environment.update({'PRISM_BROWSER_ENABLED': 'true', 'PRISM_BROWSER_BASE_URL': 'http://127.0.0.1:8319',
                            'PRISM_MANAGEMENT_KEY': key})
    config['services']['app']['environment'] = app_environment
    config['services']['prism-browser'] = {
        'image': sidecar_image, 'container_name': SIDECAR, 'restart': 'unless-stopped', 'init': True,
        'profiles': ['cutover'], 'shm_size': '1gb', 'ports': ['127.0.0.1:8319:8319'],
        'volumes': [str(data_dir) + ':/data'],
        'environment': {'PRISM_MANAGEMENT_KEY': key, 'PRISM_HOST': '0.0.0.0', 'PRISM_PORT': '8319',
                        'PRISM_DATA_DIR': '/data', 'PRISM_REQUEST_TIMEOUT': '240', 'PRISM_QUEUE_LIMIT': '8',
                        'PRISM_MAX_ACCOUNTS': '16'},
        'healthcheck': {'test': ['CMD', 'node', '-e', "fetch('http://127.0.0.1:8319/health').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"],
                        'interval': '30s', 'timeout': '5s', 'retries': 3, 'start_period': '10s'},
    }
    write_json(BACKUP / 'compose.candidate.json', config)
    print(json.dumps({'stage': 'images_ready', 'candidate': app_image, 'sidecar': sidecar_image,
                      'rollback': rollback_image, 'backup': str(BACKUP)}), flush=True)

    backup_output = run(['python3', str(BASE / 'backup_production.py')], timeout=1200)
    write_private(BACKUP / 'data-backup.json', backup_output)
    data_backup = json.loads(backup_output)
    require(data_backup.get('postgres_archive_verified'), 'Production database backup verification failed')
    print(json.dumps({'stage': 'data_backup_complete', 'backup': data_backup['backup']}), flush=True)
    require(COMPOSE.read_bytes() == state['original'] and inspect(APP)['Id'] == state['old']['Id'],
            'Production deployment changed during image preparation')
    require(sha(f"/proc/{state['old']['State']['Pid']}/exe") == state['binary_sha'], 'Running binary changed during preparation')
    compose_command = ['docker', 'compose', '--project-directory', str(BASE / 'production'),
                       '-f', str(COMPOSE), '--profile', 'cutover']

    def recreate(services):
        return run(compose_command + ['up', '-d', '--no-deps', '--wait', '--wait-timeout', '240',
                                     '--timeout', '120'] + services, merged=True, timeout=420)

    try:
        write_private(VERIFIER, state['verifier_candidate'], state['verifier_mode'])
        write_json(COMPOSE, config)
        run(compose_command + ['config', '-q'])
        write_private(BACKUP / 'prism-rollout.log', recreate(['prism-browser']))
        sidecar = inspect(SIDECAR)
        require(sidecar['State'].get('Health', {}).get('Status') == 'healthy', 'Prism process did not become healthy')
        require(sidecar['HostConfig']['PortBindings']['8319/tcp'] == [{'HostIp': '127.0.0.1', 'HostPort': '8319'}],
                'Prism port publication is not loopback-only')
        http('/health', port=8319)
        require(inspect(APP)['Id'] == state['old']['Id'], 'Starting the sidecar unexpectedly recreated the current app')
        write_private(BACKUP / 'app-rollout.log', recreate(['app']))
        current = inspect(APP)
        require(current['Image'] == inspect(app_image)['Id'], 'Running app image does not match the candidate')
        require(current['State'].get('Health', {}).get('Status') == 'healthy', 'Candidate app is unhealthy')
        before, after = environment(state['old']), environment(current)
        require({k: v for k, v in before.items() if k not in PRISM_ENV} == {k: v for k, v in after.items() if k not in PRISM_ENV},
                'An unrelated production environment variable changed')
        require(all(after.get(k) == v for k, v in {'PRISM_BROWSER_ENABLED': 'true',
                'PRISM_BROWSER_BASE_URL': 'http://127.0.0.1:8319', 'PRISM_MANAGEMENT_KEY': key}.items()),
                'Prism app environment is not configured correctly')
        require(current['Mounts'] == state['old']['Mounts'], 'Production app data mounts changed')
        require(sha(f"/proc/{current['State']['Pid']}/exe") == args.binary_sha, 'Running candidate binary checksum does not match')
        require(binary_metadata(['docker', 'exec', APP, '/app/sub2api']) == candidate_metadata, 'Running binary metadata does not match')
        for endpoint in ('/health', '/readyz'):
            http(endpoint)
        schema_after = schema()
        require(all(schema_after.get(name) == checksum for name, checksum in state['schema'].items()),
                'An existing database migration disappeared or changed checksum')
        require(all(inspect(name)['Id'] == container_id for name, container_id in state['others'].items()),
                'A preserved production dependency was recreated')
        checks = json.loads(run(['python3', str(VERIFIER)], timeout=180))
        require(checks.get('version') == args.version and checks.get('commit') == candidate_metadata['revision'],
                'Production verification does not match the candidate build')
        assets = frontend()
        logs = run(['docker', 'logs', '--since', stamp[:4] + '-' + stamp[4:6] + '-' + stamp[6:8] + 'T' +
                    stamp[9:11] + ':' + stamp[11:13] + ':' + stamp[13:15] + 'Z', APP], merged=True).decode(errors='replace')
        require(not any(line.startswith('panic:') or re.search(r'"level":"(?:fatal|panic)"|\t(?:FATAL|PANIC|DPANIC)\t', line)
                        for line in logs.splitlines()), 'Candidate app emitted a fatal startup error')
        receipt = {'version': args.version, 'revision': candidate_metadata['revision'], 'build_type': 'source',
                   'image': app_image, 'image_id': current['Image'], 'binary_sha256': args.binary_sha,
                   'sidecar_image': sidecar_image, 'sidecar_source_sha256': sidecar_source_sha,
                   'sidecar_container_id': sidecar['Id'], 'sidecar_loopback_only': True,
                   'rollback_image': rollback_image, 'backup': str(BACKUP), 'data_backup': data_backup['backup'],
                   'schema_added': sorted(set(schema_after) - set(state['schema'])),
                   'existing_migration_checksums_preserved': True, 'unrelated_environment_preserved': True,
                   'mounts_preserved': True, 'dependent_services_preserved': True,
                   'frontend': assets, 'verification_passed': True, 'model_requests_made': 0}
        write_json(BACKUP / 'receipt.json', receipt)
        write_json(stage / 'receipt.json', receipt)
        print(json.dumps(receipt, indent=2), flush=True)
    except Exception:
        write_private(COMPOSE, (BACKUP / 'compose.rollback.json').read_bytes())
        if state['sidecar']:
            write_private(BACKUP / 'rollback-prism.log', recreate(['prism-browser']))
        write_private(BACKUP / 'rollback.log', recreate(['app']))
        restored = inspect(APP)
        require(restored['State'].get('Health', {}).get('Status') == 'healthy', 'Rollback app is not healthy')
        require(binary_metadata(['docker', 'exec', APP, '/app/sub2api']) == state['metadata'], 'Rollback metadata does not match')
        require(sha(f"/proc/{restored['State']['Pid']}/exe") == state['binary_sha'], 'Rollback did not restore the exact original binary')
        require(environment(restored) == environment(state['old']), 'Rollback app environment does not match')
        require(restored['Mounts'] == state['old']['Mounts'], 'Rollback app data mounts do not match')
        for endpoint in ('/health', '/readyz'):
            http(endpoint)
        if state['sidecar'] is None and inspect(SIDECAR, optional=True) is not None:
            run(['docker', 'rm', '-f', SIDECAR])
        require(all(inspect(name)['Id'] == container_id for name, container_id in state['others'].items()),
                'A preserved production dependency changed during rollback')
        write_private(VERIFIER, state['verifier_original'], state['verifier_mode'])
        print(json.dumps({'rolled_back': True, **state['metadata'], 'rollback_image': rollback_image,
                          'backup': str(BACKUP), 'additive_migrations_retained': True}), flush=True)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--preflight-only', action='store_true')
    parser.add_argument('--binary')
    parser.add_argument('--binary-sha')
    parser.add_argument('--source')
    parser.add_argument('--version', default='0.2.14-prism.1')
    args = parser.parse_args()
    os.umask(0o077)
    state = preflight()
    print(json.dumps(public_preflight(state)), flush=True)
    if args.preflight_only:
        return
    require(bool(args.binary and args.source and args.binary_sha) and re.fullmatch(r'[0-9a-f]{64}', args.binary_sha),
            'Rollout requires --binary, --binary-sha and --source')
    rollout(args, state)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'failed': True, 'error_type': type(error).__name__, 'message': str(error)}), file=sys.stderr)
        raise SystemExit(1)
