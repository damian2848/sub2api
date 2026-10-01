"""Create a restricted online PostgreSQL backup and retain seven complete runs."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile

os.umask(0o077)
root = Path('/opt/sub2api-gpt56')
backups = root / 'backups'
backups.mkdir(mode=0o700, exist_ok=True)
stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
dest = backups / stamp
dest.mkdir(mode=0o700)
metadata = json.loads((root / 'source-artifacts/containers.json').read_text())
pg = dict(x.split('=', 1) for x in metadata['sub2api-postgres']['Config']['Env'] if '=' in x)
with (dest / 'postgres.dump').open('wb') as out:
    subprocess.run(['docker', 'exec', 'gpt56-postgres-standby', 'pg_dump', '-p', '15432',
                    '-U', pg['POSTGRES_USER'], '-d', pg['POSTGRES_DB'], '-Fc', '-Z', '6'],
                    stdout=out, stderr=subprocess.PIPE, check=True, timeout=900)
with (dest / 'postgres.dump').open('rb') as data:
    subprocess.run(['docker', 'exec', '-i', 'gpt56-postgres-standby', 'pg_restore', '--list'],
                    stdin=data, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, check=True)
subprocess.run(['docker', 'exec', 'gpt56-redis-standby', 'redis-cli', '--no-auth-warning', '-p', '16379', 'SAVE'],
                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, check=True)
shutil.copyfile(root / 'live/redis/dump.rdb', dest / 'redis.rdb')
with tarfile.open(dest / 'application-data.tar.gz', 'w:gz') as archive:
    archive.add(root / 'live/data', arcname='data', filter=lambda item: None if item.name.startswith('data/logs') else item)
configs = dest / 'config'
configs.mkdir(mode=0o700)
for relative in ('production/compose.json', 'live/compose.json', 'secrets/standby-redis.conf',
                 'cutover-receipt.json', 'production/nginx.conf'):
    shutil.copyfile(root / relative, configs / relative.replace('/', '--'))
for name in ('fullchain.pem', 'privkey.pem'):
    shutil.copyfile(Path('/etc/letsencrypt/live/gpt56-migration') / name, configs / name)
manifest = {}
for file in dest.rglob('*'):
    if file.is_file():
        with file.open('rb') as stream:
            manifest[str(file.relative_to(dest))] = hashlib.file_digest(stream, 'sha256').hexdigest()
(dest / 'SHA256.json').write_text(json.dumps(manifest, indent=2))
(dest / 'complete.json').write_text(json.dumps({'created_at': stamp, 'postgres_archive_verified': True,
    'database_and_redis_snapshots_are_independent': True, 'files': len(manifest)}))
complete = sorted(path for path in backups.iterdir() if path.is_dir() and (path / 'complete.json').exists())
for old in complete[:-7]:
    shutil.rmtree(old)
print(json.dumps({'backup': str(dest), 'postgres_archive_verified': True, 'complete_runs_retained': min(7, len(complete))}))
