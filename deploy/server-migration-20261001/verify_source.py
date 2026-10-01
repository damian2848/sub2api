"""Verify the production service is unchanged and is still the primary."""
import hashlib
import json
from pathlib import Path
import subprocess
import urllib.request

root = Path('/root/sub2api-backups/server-migration-20261001')
before = json.loads((root / 'containers.json').read_text())
names = list(before)
current = json.loads(subprocess.check_output(['docker', 'inspect', *names], text=True))
for item in current:
    assert item['Id'] == before[item['Name'].lstrip('/')]['Id'], 'Existing container was replaced'
    assert item['State']['Running']
def sql(query):
    result = subprocess.run(['docker','exec','-i','sub2api-postgres','sh','-c',
        'psql -X -q -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -At'], input=query, text=True, capture_output=True, check=True)
    return result.stdout.strip()
assert sql('SELECT pg_is_in_recovery();') == 'f'
with urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=10) as response:
    assert response.status == 200
nginx_unchanged = hashlib.sha256(Path('/etc/nginx/sites-enabled/ai-services.conf').read_bytes()).hexdigest() == hashlib.sha256((root/'nginx-source.conf').read_bytes()).hexdigest()
assert nginx_unchanged
report = {
    'existing_containers_preserved': True,
    'production_health': 200,
    'postgres_still_primary': True,
    'nginx_unchanged': nginx_unchanged,
    'primary_flush_lsn': sql('SELECT pg_current_wal_flush_lsn();'),
    'replication': json.loads(sql("SELECT COALESCE(json_agg(t),'[]') FROM (SELECT application_name,state,sync_state,pg_wal_lsn_diff(pg_current_wal_lsn(),replay_lsn) AS replay_gap_bytes FROM pg_stat_replication) t;")),
    'replication_slot': json.loads(sql("SELECT COALESCE(json_agg(t),'[]') FROM (SELECT slot_name,active,wal_status,pg_wal_lsn_diff(pg_current_wal_lsn(),restart_lsn) AS retained_wal_bytes FROM pg_replication_slots WHERE slot_name='gpt56_migration_20261001') t;")),
}
print(json.dumps(report, indent=2))
