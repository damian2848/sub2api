"""Retire the unused bootstrap app and explicitly stopped rehearsals."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import urllib.request

os.umask(0o077)
root = Path('/opt/sub2api-gpt56')
archive = root / 'cleanup' / datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
archive.mkdir(parents=True, mode=0o700)

def run(args, data=None, timeout=90):
    result = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
    if result.returncode:
        (archive / 'error.log').write_bytes(result.stdout + result.stderr)
        raise RuntimeError('Cleanup command failed; private diagnostic archived')
    return result.stdout

def inspect(names):
    return json.loads(run(['docker', 'inspect', *names]))

production = ['gpt56-production-app', 'gpt56-production-grok-video', 'gpt56-production-video-console',
              'gpt56-postgres-standby', 'gpt56-redis-standby']
before = {x['Name'].lstrip('/'): x['Id'] for x in inspect(production)}
with urllib.request.urlopen('http://127.0.0.1:18080/health', timeout=10) as response:
    assert response.status == 200
stopped = ['gpt56-preview-app', 'gpt56-preview-postgres', 'gpt56-preview-redis', 'kaiyuncode-migration-rehearsal']
bootstrap = ['sub2api', 'sub2api-postgres', 'sub2api-redis']
items = inspect(stopped + bootstrap)
for item in items:
    if item['Name'].lstrip('/') in stopped:
        assert item['State']['Status'] == 'exited'
    elif item['Name'] == '/sub2api':
        labels = item['Config'].get('Labels') or {}
        assert labels.get('com.docker.compose.project') == 'sub2api'
        assert any(m['Source'] == '/opt/sub2api/data' for m in item['Mounts'])
        assert item['Id'] == '0b28ecda4eb76cd4cc8d9becc58a856a555e1b30621f58184e0592f756bd81be', 'Bootstrap app changed; inspect again'
(archive / 'containers.json').write_text(json.dumps(items, indent=2))
pg = next(x for x in items if x['Name'] == '/sub2api-postgres')
env = dict(x.split('=', 1) for x in pg['Config']['Env'] if '=' in x)

def sql(query):
    return run(['docker', 'exec', '-i', 'sub2api-postgres', 'psql', '-X', '-q', '-U', env['POSTGRES_USER'],
                '-d', env['POSTGRES_DB'], '-v', 'ON_ERROR_STOP=1', '-At'], query.encode()).decode().strip()

assert sql("SELECT count(*) FROM usage_logs WHERE created_at>='2026-10-01 07:01:30+00';") == '0'
original_site = Path('/etc/nginx/sites-available/sub2api')
original_nginx = original_site.read_bytes()
(archive / 'nginx-bootstrap.conf').write_bytes(original_nginx)
default = '''server {
    listen 80 default_server;
    listen [::]:80 default_server;
    server_name _;
    location ^~ /.well-known/acme-challenge/ {
        root /var/lib/letsencrypt;
        default_type text/plain;
        try_files $uri =404;
    }
    location / { return 404; }
}
server {
    listen 443 ssl http2 default_server;
    listen [::]:443 ssl http2 default_server;
    server_name _;
    ssl_reject_handshake on;
}
'''
assert 'proxy_pass http://127.0.0.1:8080;' in original_nginx.decode()
original_site.write_text(default)
result = subprocess.run(['nginx', '-t'], capture_output=True)
if result.returncode:
    original_site.write_bytes(original_nginx)
    raise RuntimeError('Nginx validation failed; original restored')
run(['systemctl', 'reload', 'nginx'])
run(['docker', 'update', '--restart=no', *bootstrap])
run(['docker', 'stop', '-t', '30', 'sub2api'], timeout=60)
with (archive / 'bootstrap-postgres.dump').open('wb') as out:
    subprocess.run(['docker', 'exec', 'sub2api-postgres', 'pg_dump', '-U', env['POSTGRES_USER'],
                    '-d', env['POSTGRES_DB'], '-Fc'], stdout=out, stderr=subprocess.PIPE, check=True, timeout=300)
with (archive / 'bootstrap-postgres.dump').open('rb') as data:
    subprocess.run(['docker', 'exec', '-i', 'sub2api-postgres', 'pg_restore', '--list'], stdin=data,
                    stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, check=True)
run(['docker', 'exec', 'sub2api-redis', 'redis-cli', '--no-auth-warning', 'SAVE'])
if Path('/opt/sub2api/redis_data/dump.rdb').is_file():
    shutil.copyfile('/opt/sub2api/redis_data/dump.rdb', archive / 'bootstrap-redis.rdb')
for name in ('.env', 'docker-compose.yml', 'docker-compose.override.yml'):
    path = Path('/opt/sub2api') / name
    if path.is_file():
        shutil.copyfile(path, archive / name)
run(['docker', 'stop', '-t', '30', 'sub2api-postgres', 'sub2api-redis'], timeout=60)
final = inspect(stopped + bootstrap)
assert all(not x['State']['Running'] for x in final)
assert {x['Name'].lstrip('/'): x['Id'] for x in final} == {x['Name'].lstrip('/'): x['Id'] for x in items}
run(['docker', 'rm', *[x['Id'] for x in final]])
removed_networks = []
for name in ('gpt56-preview_isolated', 'sub2api_default'):
    result = subprocess.run(['docker', 'network', 'inspect', name], capture_output=True)
    if result.returncode == 0:
        network = json.loads(result.stdout)[0]
        if not network.get('Containers'):
            run(['docker', 'network', 'rm', name])
            removed_networks.append(name)
assert {x['Name'].lstrip('/'): x['Id'] for x in inspect(production)} == before
with urllib.request.urlopen('http://127.0.0.1:18080/health', timeout=10) as response:
    assert response.status == 200
manifest = {}
for path in archive.iterdir():
    if path.is_file():
        with path.open('rb') as stream:
            manifest[path.name] = hashlib.file_digest(stream, 'sha256').hexdigest()
(archive / 'SHA256.json').write_text(json.dumps(manifest, indent=2))
receipt = {'removed_containers': stopped + bootstrap, 'removed_empty_networks': removed_networks,
           'archive': str(archive), 'production_containers_preserved': True,
           'database_bind_mounts_and_images_preserved': True}
(archive / 'receipt.json').write_text(json.dumps(receipt, indent=2))
print(json.dumps(receipt, indent=2))
