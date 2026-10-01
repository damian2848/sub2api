"""Check the independent production without generating billable requests."""
import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import urllib.error
import urllib.request

os.umask(0o077)
root = Path('/opt/sub2api-gpt56')

def run(args, data=None, include_stderr=False):
    result = subprocess.run(args, input=data, text=True, capture_output=True, timeout=45)
    if result.returncode:
        raise RuntimeError('Verification failed: ' + ' '.join(args[:4]))
    return (result.stdout + (result.stderr if include_stderr else '')).strip()

metadata = json.loads((root / 'source-artifacts/containers.json').read_text())
pg = dict(x.split('=', 1) for x in metadata['sub2api-postgres']['Config']['Env'] if '=' in x)

def sql(query):
    return run(['docker', 'exec', '-i', 'gpt56-postgres-standby', 'psql', '-X', '-q', '-p', '15432',
                '-U', pg['POSTGRES_USER'], '-d', pg['POSTGRES_DB'], '-v', 'ON_ERROR_STOP=1', '-At'], query)

def http(path, method='GET', data=None, headers=None):
    request = urllib.request.Request('http://127.0.0.1:18080' + path, data=data, method=method, headers=headers or {})
    try:
        response = urllib.request.urlopen(request, timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.headers.get('Content-Type', ''), response.read()

database = json.loads(sql("SELECT json_build_object('in_recovery',pg_is_in_recovery(),'read_only',current_setting('default_transaction_read_only'),'server_version',current_setting('server_version'),'flush_lsn',pg_current_wal_flush_lsn());"))
assert not database['in_recovery'] and database['read_only'] == 'off'
assert not (root / 'live/postgres/standby.signal').exists()
redis = run(['docker', 'exec', 'gpt56-redis-standby', 'redis-cli', '--no-auth-warning', '-p', '16379', 'INFO', 'replication'])
redis = dict(line.split(':', 1) for line in redis.splitlines() if ':' in line)
assert redis['role'] == 'master'
assert not any(line.startswith(('replicaof ', 'masterauth ')) for line in (root / 'secrets/standby-redis.conf').read_text().splitlines())
services = ['gpt56-production-app', 'gpt56-production-grok-video', 'gpt56-production-video-console', 'gpt56-postgres-standby', 'gpt56-redis-standby']
containers = json.loads(run(['docker', 'inspect', *services]))
states = {}
for item in containers:
    assert item['State']['Running']
    health = item['State'].get('Health', {}).get('Status')
    assert health in (None, 'healthy')
    states[item['Name'].lstrip('/')] = health or 'running'
for unit in ('gpt56-data-sync.timer', 'gpt56-replication-tunnel.service', 'gpt56-bridge-tunnel.service'):
    assert run(['systemctl', 'show', unit, '-p', 'ActiveState', '--value']) == 'inactive'
checks = {}
for path in ('/health', '/readyz'):
    status, _, _ = http(path)
    assert status == 200
    checks[path] = status
report = {'checked_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
          'postgres': database, 'redis_role': redis['role'], 'containers': states,
          'http': checks, 'independent_from_old_server': True}
if '--monitor' not in __import__('sys').argv:
    status, ctype, body = http('/')
    assert status == 200 and 'text/html' in ctype and b'<div id="app"' in body
    checks['frontend'] = status
    status, ctype, _ = http('/v1/responses', 'POST', b'{}', {'Content-Type': 'application/json'})
    assert status in (401, 403) and 'application/json' in ctype
    checks['unauthenticated_api_rejected'] = status
    key = sql("SELECT value FROM settings WHERE key='admin_api_key';")
    assert key
    for path in ('/api/v1/admin/accounts?page=1&page_size=1', '/api/v1/admin/users?page=1&page_size=1'):
        status, _, _ = http(path, headers={'X-API-Key': key})
        assert status == 200
        checks[path.split('?')[0]] = status
    version = run(['docker', 'exec', 'gpt56-production-app', '/app/sub2api', '-version'], include_stderr=True)
    build = re.search(r'Sub2API (\d+\.\d+\.\d+) \(commit: ([0-9a-f]{40})', version)
    assert build, 'Production binary did not report a release version and commit'
    report['version'], report['commit'] = build.groups()
    report['data'] = json.loads(sql("SELECT json_build_object('users',(SELECT count(*) FROM users),'accounts',(SELECT count(*) FROM accounts),'api_keys',(SELECT count(*) FROM api_keys),'migrations',(SELECT count(*) FROM schema_migrations),'usage_logs',(SELECT count(*) FROM usage_logs));"))
    domains = {}
    for domain in ('sub2api.gpt56.site', 'gpt56.site', 'image2api.gpt56.site'):
        headers = run(['curl', '--fail', '--silent', '--show-error', '--max-time', '20', '--resolve', domain + ':443:127.0.0.1', '-D', '-', '-o', '/dev/null', 'https://' + domain + '/health'])
        if domain == 'gpt56.site':
            assert re.search(r'^HTTP/\S+ 308', headers, re.M)
            assert 'location: https://sub2api.gpt56.site/health' in headers.lower()
            domains[domain] = 'HTTPS 308 redirect to sub2api.gpt56.site'
        else:
            assert 'target-production' in headers
            domains[domain] = 'HTTPS 200, target production'
    for path in ('/admin/usage/async-videos/', '/admin/usage/async-videos/entry.js'):
        status = run(['curl', '--silent', '--show-error', '--max-time', '20', '--resolve', 'sub2api.gpt56.site:443:127.0.0.1', '-o', '/dev/null', '-w', '%{http_code}', 'https://sub2api.gpt56.site' + path])
        assert status == '200'
        checks[path] = int(status)
    report['domains'] = domains
    assert run(['systemctl', 'is-active', 'gpt56-tls-renew.timer']) == 'active'
    report['certificate_renewal_enabled'] = True
(root / 'production/health.json').write_text(json.dumps(report, indent=2))
print(json.dumps(report, indent=2))
