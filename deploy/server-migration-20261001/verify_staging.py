"""Historical pre-cutover verification; do not use on the promoted production."""
import datetime
import json
import os
from pathlib import Path
import subprocess
import urllib.error
import urllib.request

ROOT = Path('/opt/sub2api-gpt56')

def run(args, data=None):
    result = subprocess.run(args, input=data, capture_output=True, text=True, timeout=45)
    if result.returncode:
        raise RuntimeError('Verification command failed: ' + ' '.join(args[:4]))
    return result.stdout.strip()

metadata = json.loads((ROOT / 'source-artifacts/containers.json').read_text())
pg_env = dict(value.split('=', 1) for value in metadata['sub2api-postgres']['Config']['Env'] if '=' in value)
user, db = pg_env['POSTGRES_USER'], pg_env['POSTGRES_DB']

def sql(container, query, port='5432'):
    return run(['docker', 'exec', '-i', container, 'psql', '-X', '-q', '-h', '127.0.0.1', '-p', port, '-U', user, '-d', db, '-v', 'ON_ERROR_STOP=1', '-At'], query)

standby = sql('gpt56-postgres-standby', "SELECT json_build_object('in_recovery',pg_is_in_recovery(),'server_version',current_setting('server_version'),'receive_lsn',pg_last_wal_receive_lsn(),'replay_lsn',pg_last_wal_replay_lsn(),'receive_replay_gap_bytes',COALESCE(pg_wal_lsn_diff(pg_last_wal_receive_lsn(),pg_last_wal_replay_lsn()),0),'last_replayed_transaction',pg_last_xact_replay_timestamp(),'receiver_status',(SELECT status FROM pg_stat_wal_receiver LIMIT 1));", '15432')
standby = json.loads(standby)
assert standby['in_recovery'] and standby['receiver_status'] == 'streaming'
redis = run(['docker', 'exec', 'gpt56-redis-standby', 'redis-cli', '--no-auth-warning', '-p', '16379', 'INFO', 'replication'])
redis = dict(line.split(':', 1) for line in redis.splitlines() if ':' in line and not line.startswith('#'))
assert redis['role'] == 'slave' and redis['master_link_status'] == 'up' and redis['master_sync_in_progress'] == '0'
assert run(['systemctl', 'is-active', 'gpt56-replication-tunnel.service']) == 'active'
assert run(['systemctl', 'is-active', 'gpt56-data-sync.timer']) == 'active'
sync_exit = run(['systemctl', 'show', 'gpt56-data-sync.service', '-p', 'ExecMainStatus', '--value'])
assert sync_exit in ('0', '24')
report = {
    'checked_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'postgres': standby,
    'redis': {key: redis.get(key) for key in ('role','master_link_status','master_sync_in_progress','master_last_io_seconds_ago','slave_repl_offset','master_repl_offset')},
    'data_sync': {'timer': 'active', 'last_exit_code': int(sync_exit), 'last_completed_at': run(['systemctl', 'show', 'gpt56-data-sync.service', '-p', 'ExecMainExitTimestamp', '--value'])},
    'production_app_started': bool(run(['docker', 'ps', '-q', '--filter', 'name=^gpt56-production-app$'])),
}
assert not report['production_app_started']
os.umask(0o077)
(ROOT / 'live/sync-health.json').write_text(json.dumps(report, indent=2))

if '--monitor' not in __import__('sys').argv:
    app_network = json.loads(run(['docker', 'inspect', 'gpt56-preview-app']))[0]['NetworkSettings']['Networks']['gpt56-preview_isolated']
    preview_url = 'http://' + app_network['IPAddress'] + ':8080'
    def http(path, method='GET', data=None, headers=None):
        request = urllib.request.Request(preview_url + path, data=data, method=method, headers=headers or {})
        try:
            response = urllib.request.urlopen(request, timeout=20)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.headers.get('Content-Type', ''), response.read()
    for path in ('/health', '/readyz'):
        status, _, _ = http(path)
        assert status == 200
    status, ctype, body = http('/')
    assert status == 200 and 'text/html' in ctype and b'<div id="app"' in body
    status, ctype, _ = http('/v1/responses', 'POST', b'{}', {'Content-Type':'application/json'})
    assert status in (401, 403) and 'application/json' in ctype
    key = sql('gpt56-preview-postgres', "SELECT value FROM settings WHERE key='admin_api_key';")
    assert key
    status, _, body = http('/api/v1/admin/accounts?page=1&page_size=1', headers={'X-API-Key': key})
    assert status == 200
    account_data = json.loads(body)['data']
    network = json.loads(run(['docker', 'network', 'inspect', 'gpt56-preview_isolated']))[0]
    assert network['Internal']
    counts = json.loads(sql('gpt56-preview-postgres', "SELECT json_build_object('users',(SELECT count(*) FROM users),'accounts',(SELECT count(*) FROM accounts),'api_keys',(SELECT count(*) FROM api_keys),'migrations',(SELECT count(*) FROM schema_migrations));"))
    version = run(['docker', 'exec', 'gpt56-preview-app', '/app/sub2api', '-version'])
    assert 'Sub2API 0.2.11' in version and '7bb09518a1167295ae0a79382f27d08911404b3e' in version
    report['preview'] = {'address':preview_url, 'version':'0.2.11','commit':'7bb09518a1167295ae0a79382f27d08911404b3e','health':200,'readyz':200,'frontend':200,'admin_accounts':200,'active_accounts':account_data.get('total'),'snapshot_rows':counts,'outbound_network_isolated':True}
    (ROOT / 'migration-receipt.json').write_text(json.dumps(report, indent=2))
print(json.dumps(report, indent=2))
