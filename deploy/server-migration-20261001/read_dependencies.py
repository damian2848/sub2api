"""Print only deployment topology, never credentials."""
import json
from pathlib import Path
import subprocess

root = Path('/opt/sub2api-gpt56')
items = json.loads((root / 'source-artifacts/containers.json').read_text())
report = {}
for name, item in items.items():
    values = dict(x.split('=', 1) for x in item['Config']['Env'] if '=' in x)
    report[name] = {
        'image': item['Config']['Image'],
        'entrypoint': item['Config']['Entrypoint'],
        'mounts': [{'type': x['Type'], 'source': x['Source'], 'destination': x['Destination']} for x in item['Mounts']],
        'environment_keys': sorted(values),
        'safe_network_settings': {k: v for k, v in values.items() if k in (
            'PORT', 'HOST', 'GROK_RELAY_PORT', 'GROK_RELAY_HOST',
            'SUB2API_BASE_URL', 'SUB2API_API_URL', 'RELAY_HOST', 'RELAY_PORT',
            'SERVER_HOST', 'SERVER_PORT', 'DATABASE_HOST', 'DATABASE_PORT',
            'REDIS_HOST', 'REDIS_PORT', 'SUB2API_WS_BASE_URL') and '@' not in v},
    }
print(json.dumps(report, indent=2))
pg = dict(x.split('=', 1) for x in items['sub2api-postgres']['Config']['Env'] if '=' in x)
query = "SELECT COALESCE(json_agg(t),'[]') FROM (SELECT DISTINCT protocol,host,port FROM proxies WHERE deleted_at IS NULL) t;"
result = subprocess.run(['docker', 'exec', '-i', 'gpt56-postgres-standby',
    'psql', '-X', '-q', '-p', '15432', '-U', pg['POSTGRES_USER'], '-d', pg['POSTGRES_DB'], '-At'],
    input=query, text=True, capture_output=True, check=True)
print('Proxy network dependencies: ' + result.stdout.strip())
