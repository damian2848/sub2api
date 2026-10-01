import json
from pathlib import Path
import subprocess

root = Path('/opt/sub2api-gpt56')
meta = json.loads((root / 'source-artifacts/containers.json').read_text())
pg = dict(x.split('=', 1) for x in meta['sub2api-postgres']['Config']['Env'] if '=' in x)
query = "SELECT COALESCE(json_agg(t),'[]') FROM (SELECT state,wait_event_type,wait_event,left(query,100) AS operation FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()) t;"
result = subprocess.run(['docker', 'exec', '-i', 'gpt56-preview-postgres', 'psql', '-X', '-q',
    '-U', pg['POSTGRES_USER'], '-d', pg['POSTGRES_DB'], '-At'], input=query,
    text=True, capture_output=True, check=True)
print(result.stdout)
