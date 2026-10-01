"""Check existing key authentication and summarize logs without leaking keys."""
import collections
import json
from pathlib import Path
import re
import subprocess
import urllib.error
import urllib.request

root = Path('/opt/sub2api-gpt56')
meta = json.loads((root / 'source-artifacts/containers.json').read_text())
pg = dict(x.split('=', 1) for x in meta['sub2api-postgres']['Config']['Env'] if '=' in x)
query = "SELECT key FROM api_keys WHERE deleted_at IS NULL AND status='active' ORDER BY last_used_at DESC NULLS LAST LIMIT 10;"
result = subprocess.run(['docker', 'exec', '-i', 'gpt56-postgres-standby', 'psql', '-X', '-q', '-p', '15432',
    '-U', pg['POSTGRES_USER'], '-d', pg['POSTGRES_DB'], '-At'], input=query, text=True, capture_output=True, check=True)
statuses = []
success = False
for key in result.stdout.splitlines():
    request = urllib.request.Request('http://127.0.0.1:18080/v1/models', headers={'Authorization': 'Bearer ' + key})
    try:
        response = urllib.request.urlopen(request, timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        statuses.append(response.status)
        if response.status == 200:
            payload = json.loads(response.read())
            count = len(payload.get('data', []))
            success = True
            break
assert success, 'No existing active key could list models; investigate authentication'
access = collections.Counter()
for line in Path('/var/log/nginx/access.log').read_text(errors='replace').splitlines()[-500:]:
    match = re.search(r'"(GET|POST|HEAD|PUT|DELETE|OPTIONS) ([^ ]+) HTTP/[^\"]+" (\d+)', line)
    if match:
        method, path, status = match.groups()
        path = path.split('?', 1)[0]
        if path in ('/responses', '/v1/responses', '/models', '/v1/models', '/v1/videos', '/health'):
            access[(method, path, int(status))] += 1
print(json.dumps({'existing_api_key_authentication': 200, 'listed_models': count,
    'model_generations_issued_by_verification': 0,
    'recent_access_counts': [{'method': m, 'path': p, 'status': s, 'count': n} for (m,p,s),n in access.items()]}, indent=2))
