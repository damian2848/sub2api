import json
import os
from pathlib import Path
import secrets
import subprocess

os.umask(0o077)
root = Path('/opt/sub2api-gpt56')
if root.exists():
    raise RuntimeError('Migration directory already exists; inspect before reusing it')
for name in ('ssh', 'secrets', 'source-artifacts', 'live/data', 'live/postgres', 'live/redis', 'preview/data', 'release', 'production'):
    (root / name).mkdir(parents=True, exist_ok=True, mode=0o700)
public = {}
for name in ('tunnel', 'data-sync', 'artifact-sync'):
    path = root / 'ssh' / name
    subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', 'gpt56-migration-' + name, '-f', str(path)], check=True)
    public[name] = path.with_suffix('.pub').read_text().strip()
(root / 'ssh/known_hosts').write_text(PAYLOAD['known_hosts'])
password = secrets.token_hex(32)
(root / 'secrets/replication-password').write_text(password + '\n')
print(json.dumps({'public_keys': public, 'replication_password': password}))
