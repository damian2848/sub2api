"""Continue after a verified physical backup without recreating its slot."""
from pathlib import Path
import json
import subprocess

root = Path('/opt/sub2api-gpt56')
source = (root / 'configure_target.py').read_text()
namespace = {}
exec(source.split('unit = ', 1)[0], namespace)
artifacts = root / 'source-artifacts'
images = json.loads((artifacts / 'images.json').read_text())
pgdata = root / 'live/postgres'
subprocess.run(['docker', 'run', '--rm', '--user', '70:70',
                '-v', str(pgdata) + ':/var/lib/postgresql/data:ro',
                '--entrypoint', 'pg_verifybackup', images['sub2api-postgres']['tag'],
                '/var/lib/postgresql/data'], check=True)
containers = json.loads((artifacts / 'containers.json').read_text())
env = lambda name: dict(item.split('=', 1) for item in containers[name]['Config']['Env'] if '=' in item)
namespace.update(images=images, pgdata=pgdata, containers=containers, env=env,
                 source_app=env('sub2api'), source_pg=env('sub2api-postgres'),
                 source_redis=env('sub2api-redis'), pgpass=root / 'secrets/replication.pgpass')
assert not (root / 'live/compose.json').exists(), 'Already resumed; inspect current phase'
exec('redis_password = ' + source.split('redis_password = ', 1)[1], namespace)
