"""Verify the final checkpoint, promote once, and serve the prepared production."""
import datetime
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.request

os.umask(0o077)
root = Path('/opt/sub2api-gpt56')
assert (root / 'migration-receipt.json').exists(), 'Preview verification is required'
receipt = json.loads((root / 'source-final.json').read_text())
metadata = json.loads((root / 'source-artifacts/containers.json').read_text())
env = dict(x.split('=', 1) for x in metadata['sub2api-postgres']['Config']['Env'] if '=' in x)

def run(args, data=None, timeout=180):
    result = subprocess.run(args, input=data, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        (root / 'cutover-error.log').write_text(result.stdout + result.stderr)
        raise RuntimeError('Cutover command failed; inspect private cutover-error.log')
    return result.stdout.strip()

def sql(query):
    return run(['docker', 'exec', '-i', 'gpt56-postgres-standby', 'psql', '-X', '-q', '-p', '15432',
                '-U', env['POSTGRES_USER'], '-d', env['POSTGRES_DB'], '-v', 'ON_ERROR_STOP=1', '-At'], query)

assert sql('SELECT pg_is_in_recovery();') == 't', 'Already promoted; inspect production instead'
run(['systemctl', 'disable', '--now', 'gpt56-data-sync.timer'])
run(['systemctl', 'start', 'gpt56-data-sync.service'])
deadline = time.monotonic() + 90
while time.monotonic() < deadline:
    if sql("SELECT pg_last_wal_replay_lsn()>='%s'::pg_lsn;" % receipt['final_flush_lsn']) == 't':
        break
    time.sleep(1)
else:
    raise RuntimeError('Final PostgreSQL checkpoint not replayed; source remains fenced')
assert json.loads(sql(receipt['fingerprint_sql'])) == receipt['data'], 'Final data fingerprints differ'
deadline = time.monotonic() + 45
while time.monotonic() < deadline:
    info = run(['docker', 'exec', 'gpt56-redis-standby', 'redis-cli', '--no-auth-warning', '-p', '16379', 'INFO', 'replication'])
    redis = dict(line.split(':', 1) for line in info.splitlines() if ':' in line)
    if redis.get('master_link_status') == 'up' and int(redis['slave_repl_offset']) >= receipt['redis_offset']:
        break
    time.sleep(1)
else:
    raise RuntimeError('Redis final checkpoint not synchronized')
receipt['target_replay_lsn_before_promotion'] = sql('SELECT pg_last_wal_replay_lsn();')
receipt['target_redis_offset_before_promotion'] = int(redis['slave_repl_offset'])
receipt['final_data_fingerprints_match'] = True
assert sql('SELECT pg_promote(true,60);') == 't'
assert sql('SELECT pg_is_in_recovery();') == 'f'
sql("ALTER SYSTEM RESET primary_conninfo;")
sql("ALTER SYSTEM RESET primary_slot_name;")
sql("ALTER SYSTEM SET default_transaction_read_only='off';")
sql('SELECT pg_reload_conf();')
conf = root / 'secrets/standby-redis.conf'
text = '\n'.join(line for line in conf.read_text().splitlines() if not line.startswith(('replicaof ', 'masterauth ', 'replica-read-only '))) + '\n'
conf.write_text(text)
os.chown(conf, 999, 1000)
assert run(['docker', 'exec', 'gpt56-redis-standby', 'redis-cli', '--no-auth-warning', '-p', '16379', 'REPLICAOF', 'NO', 'ONE']) == 'OK'
run(['docker', 'exec', 'gpt56-redis-standby', 'redis-cli', '--no-auth-warning', '-p', '16379', 'SAVE'])
receipt['promoted_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
(root / 'cutover-receipt.json').write_text(json.dumps(receipt, indent=2))
production = root / 'production/compose.json'
compose = json.loads(production.read_text())
app = compose['services']['app']
app['healthcheck'] = {'test': ['CMD', 'curl', '-fsS', 'http://127.0.0.1:18080/health'], 'interval': '5s', 'timeout': '5s', 'retries': 24}
app['environment']['SERVER_GRACEFUL_SHUTDOWN_TIMEOUT'] = '240'
app['stop_grace_period'] = '250s'
production.write_text(json.dumps(compose, indent=2))
run(['docker', 'compose', '-f', str(production), '--profile', 'cutover', 'up', '-d', '--wait', '--wait-timeout', '180'], timeout=240)
for endpoint in ('/health', '/readyz'):
    with urllib.request.urlopen('http://127.0.0.1:18080' + endpoint, timeout=10) as response:
        assert response.status == 200
active = Path('/etc/nginx/sites-available/gpt56-migration.conf')
(root / 'production/nginx-bridge-before.conf').write_bytes(active.read_bytes())
active.write_bytes((root / 'production/nginx.conf').read_bytes())
run(['nginx', '-t'])
run(['systemctl', 'reload', 'nginx'])
receipt['target_serving_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
receipt['version'] = '0.2.11'
receipt['domains'] = ['sub2api.gpt56.site', 'gpt56.site', 'image2api.gpt56.site']
(root / 'cutover-receipt.json').write_text(json.dumps(receipt, indent=2))
print(json.dumps({k: v for k, v in receipt.items() if k != 'fingerprint_sql'}, indent=2))
