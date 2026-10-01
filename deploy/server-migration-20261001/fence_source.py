"""Drain the old entrypoint and stop every application writer before promotion."""
import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import time

os.umask(0o077)
root = Path('/root/sub2api-backups/server-migration-20261001')
assert not (root / 'cutover-final.json').exists(), 'Source was already fenced; inspect receipt'
path = Path('/etc/nginx/sites-enabled/ai-services.conf').resolve()
source = path.read_text()
(root / 'nginx-before-cutover.conf').write_text(source)
maintenance = re.sub(r'proxy_pass\s+[^;]+;', 'add_header Retry-After 5 always;\n        return 503;', source)
path.write_text(maintenance)
subprocess.run(['nginx', '-t'], check=True)
subprocess.run(['systemctl', 'reload', 'nginx'], check=True)
started = datetime.datetime.now(datetime.timezone.utc).isoformat()
# Existing Nginx workers may complete their active upstream streams.
deadline = time.monotonic() + 30
while time.monotonic() < deadline:
    workers = subprocess.check_output(['ps', '-C', 'nginx', '-o', 'args='], text=True)
    if 'worker process is shutting down' not in workers:
        break
    time.sleep(1)
names = ['sub2api', 'grok-video-adapter', 'sub2api-async-video-console']
subprocess.run(['docker', 'update', '--restart=no', *names], check=True, stdout=subprocess.DEVNULL)
subprocess.run(['docker', 'stop', '-t', '60', *names], check=True, stdout=subprocess.DEVNULL, timeout=90)
assert all(not item['State']['Running'] for item in json.loads(subprocess.check_output(['docker', 'inspect', *names])))

def sql(query):
    result = subprocess.run(['docker', 'exec', '-i', 'sub2api-postgres', 'sh', '-c',
        'psql -X -q -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -At'],
        input=query, text=True, capture_output=True, check=True)
    return result.stdout.strip()

sql("ALTER SYSTEM SET default_transaction_read_only='on';")
sql('SELECT pg_reload_conf();')
assert sql('SHOW default_transaction_read_only;') == 'on'
sql("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid();")
assert sql("SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid();") == '0'
subprocess.run(['docker', 'exec', 'sub2api-redis', 'redis-cli', '--no-auth-warning', 'CLIENT', 'PAUSE', '300000', 'WRITE'], check=True, stdout=subprocess.DEVNULL)
redis = subprocess.check_output(['docker', 'exec', 'sub2api-redis', 'redis-cli', '--no-auth-warning', 'INFO', 'replication'], text=True)
redis = dict(line.split(':', 1) for line in redis.splitlines() if ':' in line)
query = """SELECT json_build_object(
 'users',(SELECT count(*) FROM users),
 'accounts',(SELECT count(*) FROM accounts),
 'api_keys',(SELECT count(*) FROM api_keys),
 'migrations',(SELECT count(*) FROM schema_migrations),
 'usage_logs',(SELECT count(*) FROM usage_logs),
 'users_digest',(SELECT md5(string_agg(md5(row_to_json(t)::text),'' ORDER BY id)) FROM users t),
 'accounts_digest',(SELECT md5(string_agg(md5(row_to_json(t)::text),'' ORDER BY id)) FROM accounts t),
 'keys_digest',(SELECT md5(string_agg(md5(row_to_json(t)::text),'' ORDER BY id)) FROM api_keys t),
 'settings_digest',(SELECT md5(string_agg(md5(row_to_json(t)::text),'' ORDER BY key)) FROM settings t));"""
receipt = {'maintenance_started_at': started, 'source_writers_stopped': names,
           'source_database_read_only': True, 'final_flush_lsn': sql('SELECT pg_current_wal_flush_lsn();'),
           'redis_offset': int(redis['master_repl_offset']), 'data': json.loads(sql(query)),
           'fingerprint_sql': query, 'fenced_at': datetime.datetime.now(datetime.timezone.utc).isoformat()}
(root / 'cutover-final.json').write_text(json.dumps(receipt, indent=2))
print(json.dumps({k: v for k, v in receipt.items() if k != 'fingerprint_sql'}, indent=2))
