"""Install continuously replicated standbys and a separate, isolated preview."""
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tarfile
import time
import urllib.request

os.umask(0o077)
root = Path('/opt/sub2api-gpt56')
artifacts = root / 'source-artifacts'

def run(args, data=None, timeout=600):
    result = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
    if result.returncode:
        (root / 'command-error.log').write_bytes(result.stdout + result.stderr)
        raise RuntimeError('Command failed: ' + ' '.join(args[:3]) + '; private diagnostic: ' + str(root / 'command-error.log'))
    return result.stdout

def write(path, text, mode=0o600, owner=None):
    path = Path(path)
    path.write_text(text)
    path.chmod(mode)
    if owner:
        os.chown(path, *owner)

def ssh_options(key):
    return ['ssh', '-i', str(root / 'ssh' / key), '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
            '-o', 'StrictHostKeyChecking=yes', '-o', 'UserKnownHostsFile=' + str(root / 'ssh/known_hosts'), '-o', 'ConnectTimeout=15']

unit = '''[Unit]
Description=Gpt56 encrypted live PostgreSQL and Redis replication tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/ssh -NT -i /opt/sub2api-gpt56/ssh/tunnel -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/opt/sub2api-gpt56/ssh/known_hosts -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -L 127.0.0.1:25432:172.19.0.3:5432 -L 127.0.0.1:26379:172.19.0.2:6379 root@43.128.10.245
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
'''
write('/etc/systemd/system/gpt56-replication-tunnel.service', unit, 0o644)
run(['systemctl', 'daemon-reload'])
run(['systemctl', 'enable', '--now', 'gpt56-replication-tunnel.service'])
run(['rsync', '-a', '--bwlimit=20480', '--exclude=authorized_keys.before', '-e', shlex.join(ssh_options('artifact-sync')),
     'root@43.128.10.245:./', str(artifacts) + '/'], timeout=1800)
for name, expected in json.loads((artifacts / 'SHA256.json').read_text()).items():
    if name == 'authorized_keys.before':
        continue
    with (artifacts / name).open('rb') as stream:
        assert hashlib.file_digest(stream, 'sha256').hexdigest() == expected, 'Transferred artifact checksum mismatch'
print(json.dumps({'phase': 'backups-transferred-and-verified'}), flush=True)
run(['docker', 'load', '-i', str(artifacts / 'runtime-images.tar.gz')], timeout=1800)
images = json.loads((artifacts / 'images.json').read_text())
for item in images.values():
    assert json.loads(run(['docker', 'image', 'inspect', item['tag']]))[0]['Id'] == item['id']
containers = json.loads((artifacts / 'containers.json').read_text())
env = lambda name: dict(item.split('=', 1) for item in containers[name]['Config']['Env'] if '=' in item)
source_app = env('sub2api')
source_pg = env('sub2api-postgres')
source_redis = env('sub2api-redis')
password = (root / 'secrets/replication-password').read_text().strip()
pgpass = root / 'secrets/replication.pgpass'
write(pgpass, f'127.0.0.1:25432:replication:gpt56_migration_repl:{password}\n', owner=(70, 70))
pgdata = root / 'live/postgres'
os.chown(pgdata, 70, 70)
assert not any(pgdata.iterdir()), 'Standby data directory is not empty'
run(['docker', 'run', '--rm', '--network', 'host', '--user', '70:70',
     '-v', str(pgdata) + ':/var/lib/postgresql/data', '-v', str(pgpass) + ':/run/replication.pgpass:ro',
     '--entrypoint', 'pg_basebackup', images['sub2api-postgres']['tag'],
     '-d', 'host=127.0.0.1 port=25432 user=gpt56_migration_repl passfile=/run/replication.pgpass application_name=gpt56-migration-standby',
     '-D', '/var/lib/postgresql/data', '-Fp', '-X', 'stream', '-R', '-C', '-S', 'gpt56_migration_20261001',
     '--checkpoint=spread', '--max-rate=32M', '--no-password'], timeout=1800)
print(json.dumps({'phase': 'postgres-physical-basebackup-complete'}), flush=True)
redis_password = source_redis.get('REDISCLI_AUTH', source_app.get('REDIS_PASSWORD', ''))
redis_conf = 'bind 127.0.0.1\nport 16379\nprotected-mode yes\ndir /data\nappendonly yes\nreplica-read-only yes\nreplicaof 127.0.0.1 26379\n'
if redis_password:
    redis_conf += 'requirepass ' + json.dumps(redis_password) + '\nmasterauth ' + json.dumps(redis_password) + '\n'
write(root / 'secrets/standby-redis.conf', redis_conf, owner=(999, 1000))
os.chown(root / 'live/redis', 999, 1000)
write(root / 'secrets/redis.env', 'REDISCLI_AUTH=' + redis_password + '\n')
compose = {
    'name': 'gpt56-live-sync',
    'services': {
        'postgres': {
            'image': images['sub2api-postgres']['tag'], 'container_name': 'gpt56-postgres-standby',
            'network_mode': 'host', 'restart': 'unless-stopped', 'user': '70:70',
            'entrypoint': ['postgres'], 'command': ['-D', '/var/lib/postgresql/data', '-c', 'port=15432', '-c', 'listen_addresses=127.0.0.1', '-c', 'hot_standby=on'],
            'volumes': [str(pgdata) + ':/var/lib/postgresql/data', str(pgpass) + ':/run/replication.pgpass:ro'],
            'healthcheck': {'test': ['CMD', 'pg_isready', '-h', '127.0.0.1', '-p', '15432', '-U', source_pg['POSTGRES_USER']], 'interval': '10s', 'timeout': '5s', 'retries': 6},
        },
        'redis': {
            'image': images['sub2api-redis']['tag'], 'container_name': 'gpt56-redis-standby',
            'network_mode': 'host', 'restart': 'unless-stopped', 'user': '999:1000',
            'command': ['redis-server', '/run/redis.conf'],
            'env_file': [str(root / 'secrets/redis.env')],
            'volumes': [str(root / 'live/redis') + ':/data', str(root / 'secrets/standby-redis.conf') + ':/run/redis.conf:ro'],
            'healthcheck': {'test': ['CMD', 'redis-cli', '--no-auth-warning', '-p', '16379', 'ping'], 'interval': '10s', 'timeout': '5s', 'retries': 6},
        },
    },
}
write(root / 'live/compose.json', json.dumps(compose, indent=2))
run(['docker', 'compose', '-f', str(root / 'live/compose.json'), 'up', '-d', '--wait', '--wait-timeout', '180'], timeout=240)
sync_command = ['/usr/bin/rsync', '-a', '--delete-delay', '--bwlimit=10240', '--exclude=/logs/', '-e', shlex.join(ssh_options('data-sync')),
                'root@43.128.10.245:./', str(root / 'live/data') + '/']
sync = '''[Unit]
Description=Gpt56 live application files sync (one-way from production)
After=network-online.target

[Service]
Type=oneshot
UMask=0077
ExecStart=''' + shlex.join(sync_command).replace('%', '%%') + '''
SuccessExitStatus=24
Nice=15
IOSchedulingClass=best-effort
IOSchedulingPriority=7
'''
write('/etc/systemd/system/gpt56-data-sync.service', sync, 0o644)
write('/etc/systemd/system/gpt56-data-sync.timer', '''[Unit]
Description=Keep Gpt56 application files synchronized every minute

[Timer]
OnBootSec=30s
OnUnitInactiveSec=60s
Persistent=true

[Install]
WantedBy=timers.target
''', 0o644)
run(['systemctl', 'daemon-reload'])
run(['systemctl', 'start', 'gpt56-data-sync.service'], timeout=600)
run(['systemctl', 'enable', '--now', 'gpt56-data-sync.timer'])
print(json.dumps({'phase': 'continuous-postgres-redis-and-file-sync-running'}), flush=True)
archive = root / 'release/sub2api_0.2.11_linux_amd64.tar.gz'
urllib.request.urlretrieve('https://github.com/damian2848/sub2api/releases/download/v0.2.11/sub2api_0.2.11_linux_amd64.tar.gz', archive)
with archive.open('rb') as stream:
    assert hashlib.file_digest(stream, 'sha256').hexdigest() == '69f930649354b07497197de3eb497fb1f098c197462a6059ade3fa2f1bea33db'
with tarfile.open(archive) as source:
    member = source.getmember('sub2api')
    binary = root / 'release/sub2api'
    with source.extractfile(member) as reader, binary.open('wb') as output:
        shutil.copyfileobj(reader, output)
    binary.chmod(0o755)
with tarfile.open(root / 'release/resources.tar.gz') as source:
    source.extractall(root / 'release', filter='data')
write(root / 'release/Dockerfile', 'FROM ' + images['sub2api']['tag'] + '\nCOPY --chown=1000:1000 sub2api /app/sub2api\nCOPY --chown=1000:1000 backend/resources /app/resources\nLABEL org.opencontainers.image.version="0.2.11"\nLABEL org.opencontainers.image.revision="7bb09518a1167295ae0a79382f27d08911404b3e"\nLABEL org.opencontainers.image.source="https://github.com/damian2848/sub2api"\n')
write(root / 'release/.dockerignore', '*\n!Dockerfile\n!sub2api\n!backend\n!backend/resources\n!backend/resources/**\n')
image = 'sub2api-gpt56:0.2.11-7bb09518a'
run(['docker', 'build', '--pull=false', '-t', image, str(root / 'release')], timeout=600)
version = run(['docker', 'run', '--rm', '--network', 'none', '--entrypoint', '/app/sub2api', image, '-version']).decode()
assert 'Sub2API 0.2.11' in version and '7bb09518a1167295ae0a79382f27d08911404b3e' in version
shutil.copytree(root / 'live/data', root / 'preview/data', dirs_exist_ok=True)
preview_app = dict(source_app)
preview_app.update({'DATABASE_HOST': 'postgres', 'DATABASE_PORT': '5432', 'REDIS_HOST': 'redis', 'REDIS_PORT': '6379', 'SERVER_HOST': '0.0.0.0', 'SERVER_PORT': '8080', 'RUNTIME_ROLE': 'full', 'TICKET_ENABLED': 'false'})
preview_pg = {key: source_pg[key] for key in ('POSTGRES_USER', 'POSTGRES_PASSWORD', 'POSTGRES_DB')}
preview_pg['PGDATA'] = '/var/lib/postgresql/data'
preview_redis = 'bind 0.0.0.0\nport 6379\nprotected-mode no\ndir /data\n'
if redis_password:
    preview_redis += 'requirepass ' + json.dumps(redis_password) + '\n'
write(root / 'secrets/preview-redis.conf', preview_redis, owner=(999, 1000))
preview = {
    'name': 'gpt56-preview', 'networks': {'isolated': {'internal': True}},
    'services': {
        'postgres': {'image': images['sub2api-postgres']['tag'], 'container_name': 'gpt56-preview-postgres', 'restart': 'unless-stopped', 'environment': preview_pg, 'networks': ['isolated'], 'volumes': [str(root / 'preview/postgres') + ':/var/lib/postgresql/data'], 'healthcheck': {'test': ['CMD-SHELL', 'pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"'], 'interval': '5s', 'timeout': '5s', 'retries': 12}},
        'redis': {'image': images['sub2api-redis']['tag'], 'container_name': 'gpt56-preview-redis', 'restart': 'unless-stopped', 'networks': ['isolated'], 'command': ['redis-server', '/run/redis.conf'], 'environment': {'REDISCLI_AUTH': redis_password}, 'volumes': [str(root / 'secrets/preview-redis.conf') + ':/run/redis.conf:ro'], 'healthcheck': {'test': ['CMD', 'redis-cli', '--no-auth-warning', 'ping'], 'interval': '5s', 'timeout': '5s', 'retries': 12}},
        'app': {'image': image, 'container_name': 'gpt56-preview-app', 'restart': 'unless-stopped', 'environment': preview_app, 'networks': ['isolated'], 'ports': ['127.0.0.1:18090:8080'], 'volumes': [str(root / 'preview/data') + ':/app/data'], 'depends_on': {'postgres': {'condition': 'service_healthy'}, 'redis': {'condition': 'service_healthy'}}},
    },
}
write(root / 'preview/compose.json', json.dumps(preview, indent=2))
preview_cmd = ['docker', 'compose', '-f', str(root / 'preview/compose.json')]
run(preview_cmd + ['up', '-d', '--wait', '--wait-timeout', '180', 'postgres', 'redis'], timeout=240)
with (artifacts / 'postgres.dump').open('rb') as backup:
    result = subprocess.run(['docker', 'exec', '-i', 'gpt56-preview-postgres', 'pg_restore', '--no-owner', '--exit-on-error', '-U', source_pg['POSTGRES_USER'], '-d', source_pg['POSTGRES_DB']], stdin=backup, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=900)
    if result.returncode:
        (root / 'preview/restore-error.log').write_bytes(result.stderr)
        raise RuntimeError('Isolated preview database restore failed')
run(preview_cmd + ['up', '-d', '--wait', '--wait-timeout', '240', 'app'], timeout=300)
production_env = dict(source_app)
production_env.update({'DATABASE_HOST': '127.0.0.1', 'DATABASE_PORT': '15432', 'REDIS_HOST': '127.0.0.1', 'REDIS_PORT': '16379', 'SERVER_HOST': '127.0.0.1', 'SERVER_PORT': '18080', 'RUNTIME_ROLE': 'full'})
production = {'name': 'gpt56-production', 'services': {'app': {'image': image, 'container_name': 'gpt56-production-app', 'profiles': ['cutover'], 'restart': 'unless-stopped', 'network_mode': 'host', 'environment': production_env, 'volumes': [str(root / 'live/data') + ':/app/data']}}}
for name, service in [('grok-video-adapter', 'grok-video'), ('sub2api-async-video-console', 'video-console')]:
    helper = env(name)
    for key in ('SUB2API_BASE_URL', 'SUB2API_API_URL'):
        if key in helper:
            helper[key] = 'http://127.0.0.1:18080'
    if 'REDIS_URL' in helper:
        from urllib.parse import urlsplit, urlunsplit
        parsed = urlsplit(helper['REDIS_URL'])
        auth = parsed.netloc.rsplit('@', 1)[0] + '@' if '@' in parsed.netloc else ''
        helper['REDIS_URL'] = urlunsplit((parsed.scheme, auth + '127.0.0.1:16379', parsed.path, parsed.query, parsed.fragment))
    production['services'][service] = {'image': images[name]['tag'], 'container_name': 'gpt56-production-' + service, 'profiles': ['cutover'], 'restart': 'unless-stopped', 'network_mode': 'host', 'environment': helper}
write(root / 'production/compose.json', json.dumps(production, indent=2))
run(['docker', 'compose', '-f', str(root / 'production/compose.json'), '--profile', 'cutover', 'config', '-q'])
print(json.dumps({'phase': 'isolated-v0.2.11-preview-ready', 'preview_port': 18090, 'production_still_disabled': True}), flush=True)
