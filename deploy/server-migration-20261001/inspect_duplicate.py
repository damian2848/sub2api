"""Check the bootstrap deployment's usage before classifying it as obsolete."""
import json
from pathlib import Path
import subprocess

meta = json.loads(subprocess.check_output(['docker', 'inspect', 'sub2api', 'sub2api-postgres'], text=True))
pg = next(x for x in meta if x['Name'] == '/sub2api-postgres')
env = dict(x.split('=', 1) for x in pg['Config']['Env'] if '=' in x)
query = """SELECT json_build_object(
 'users',(SELECT count(*) FROM users WHERE deleted_at IS NULL),
 'accounts',(SELECT count(*) FROM accounts WHERE deleted_at IS NULL),
 'api_keys',(SELECT count(*) FROM api_keys WHERE deleted_at IS NULL),
 'usage_logs',(SELECT count(*) FROM usage_logs),
 'last_usage',(SELECT max(created_at) FROM usage_logs),
 'usage_since_migration',(SELECT count(*) FROM usage_logs WHERE created_at>='2026-10-01 07:01:30+00'));
"""
result = subprocess.run(['docker', 'exec', '-i', 'sub2api-postgres', 'psql', '-X', '-q',
    '-U', env['POSTGRES_USER'], '-d', env['POSTGRES_DB'], '-At'], input=query,
    text=True, capture_output=True, check=True)
print('Bootstrap deployment: ' + result.stdout.strip())
for name in ('sub2api.service', 'gpt56-sync-monitor.service'):
    output = subprocess.run(['systemctl', 'show', name, '-p', 'LoadState', '-p', 'ActiveState'], capture_output=True, text=True)
    print(name + ': ' + output.stdout.strip().replace('\n', ', '))
