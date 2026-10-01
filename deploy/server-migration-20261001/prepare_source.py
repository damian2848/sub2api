"""Create online backups and narrowly scoped replication access."""
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess

os.umask(0o077)
root = Path('/root/sub2api-backups/server-migration-20261001')
root.mkdir(mode=0o700, exist_ok=False)

def run(args, data=None, timeout=120):
    result = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
    if result.returncode:
        (root / 'command-error.log').write_bytes(result.stderr)
        raise RuntimeError('Command failed; private diagnostic: ' + str(root / 'command-error.log'))
    return result.stdout

def sql(text):
    return run(['docker', 'exec', '-i', 'sub2api-postgres', 'sh', '-c',
                'psql -X -q -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -At'], text.encode()).decode().strip()

def inspect(name):
    return json.loads(run(['docker', 'inspect', name]))[0]

containers = {name: inspect(name) for name in ('sub2api', 'sub2api-postgres', 'sub2api-redis', 'grok-video-adapter', 'sub2api-async-video-console')}
assert containers['sub2api']['State']['Health']['Status'] == 'healthy'
assert sql('SELECT count(*) FROM pg_replication_slots;') == '0', 'Unexpected existing replication slots'
user = 'gpt56_migration_repl'
password = PAYLOAD['replication_password']
assert re.fullmatch('[a-f0-9]{64}', password)
sql(f"CREATE ROLE {user} WITH LOGIN REPLICATION CONNECTION LIMIT 3 PASSWORD '{password}';")
sql("ALTER SYSTEM SET max_slot_wal_keep_size='2GB'; SELECT pg_reload_conf();")
pgdata = sql('SHOW data_directory;')
hba = run(['docker', 'exec', 'sub2api-postgres', 'cat', pgdata + '/pg_hba.conf'])
(root / 'pg_hba.before.conf').write_bytes(hba)
line = f'\n# gpt56 online migration (20261001)\nhost replication {user} 172.19.0.1/32 scram-sha-256\n'
run(['docker', 'exec', '-i', 'sub2api-postgres', 'sh', '-c', 'cat >> "$PGDATA/pg_hba.conf"'], line.encode())
sql('SELECT pg_reload_conf();')
keys = Path('/root/.ssh/authorized_keys')
before = keys.read_bytes()
(root / 'authorized_keys.before').write_bytes(before)
entries = [
    'from="23.132.132.111",restrict,port-forwarding,permitopen="172.19.0.3:5432",permitopen="172.19.0.2:6379",command="/bin/false" ' + PAYLOAD['public_keys']['tunnel'],
    'from="23.132.132.111",restrict,command="/usr/bin/rrsync -ro /home/ubuntu/sub2api/deploy/data" ' + PAYLOAD['public_keys']['data-sync'],
    'from="23.132.132.111",restrict,command="/usr/bin/rrsync -ro /root/sub2api-backups/server-migration-20261001" ' + PAYLOAD['public_keys']['artifact-sync'],
]
with keys.open('ab') as stream:
    stream.write(('\n' + '\n'.join(entries) + '\n').encode())
keys.chmod(0o600)
(root / 'containers.json').write_text(json.dumps(containers))
deploy = Path('/home/ubuntu/sub2api/deploy')
for name in ('.env', 'docker-compose.local.yml'):
    shutil.copy2(deploy / name, root / name)
(root / 'schema-migrations.json').write_text(sql('SELECT json_agg(t ORDER BY filename) FROM schema_migrations t;'))
nginx = Path('/etc/nginx/sites-enabled/ai-services.conf')
(root / 'nginx-source.conf').write_bytes(nginx.read_bytes())
tls = root / 'tls'
tls.mkdir()
for file in set(re.findall(r'\bssl_certificate(?:_key)?\s+([^;]+);', nginx.read_text())):
    file = Path(file)
    if file.is_file():
        dest = tls / file.name
        if dest.exists() and dest.read_bytes() != file.read_bytes():
            dest = tls / (hashlib.sha256(str(file).encode()).hexdigest()[:8] + '-' + file.name)
        shutil.copy2(file, dest)
        dest.chmod(0o600)
print(json.dumps({'phase': 'replication-access-ready', 'backup': str(root)}), flush=True)
with (root / 'postgres.dump').open('wb') as output:
    process = subprocess.run(['docker', 'exec', 'sub2api-postgres', 'sh', '-c',
        'nice -n 15 pg_dump -Fc -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner'], stdout=output, stderr=subprocess.PIPE, timeout=1800)
    if process.returncode:
        raise RuntimeError('Online pg_dump failed; live services were not stopped')
print(json.dumps({'phase': 'online-database-backup-complete', 'bytes': (root / 'postgres.dump').stat().st_size}), flush=True)
images = {}
for name, tag in {
    'sub2api': 'sub2api-migration/runtime:source-20261001',
    'sub2api-postgres': 'sub2api-migration/postgres:source-20261001',
    'sub2api-redis': 'sub2api-migration/redis:source-20261001',
    'grok-video-adapter': 'sub2api-migration/grok-video:source-20261001',
    'sub2api-async-video-console': 'sub2api-migration/video-console:source-20261001',
}.items():
    run(['docker', 'image', 'tag', containers[name]['Image'], tag])
    images[name] = {'tag': tag, 'id': containers[name]['Image']}
(root / 'images.json').write_text(json.dumps(images))
with (root / 'runtime-images.tar.gz').open('wb') as output:
    exporter = subprocess.Popen(['nice', '-n', '15', 'docker', 'save', *[item['tag'] for item in images.values()]], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    compressor = subprocess.Popen(['nice', '-n', '15', 'gzip', '-1'], stdin=exporter.stdout, stdout=output, stderr=subprocess.PIPE)
    exporter.stdout.close()
    _, error = compressor.communicate(timeout=1800)
    if compressor.returncode or exporter.wait(timeout=60):
        raise RuntimeError('Runtime image export failed')
manifest = {file.name: hashlib.sha256(file.read_bytes()).hexdigest() for file in root.iterdir() if file.is_file()}
(root / 'SHA256.json').write_text(json.dumps(manifest))
print(json.dumps({'phase': 'source-preparation-complete', 'image_archive_bytes': (root / 'runtime-images.tar.gz').stat().st_size, 'live_health': inspect('sub2api')['State']['Health']['Status']}), flush=True)
