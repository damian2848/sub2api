"""Report container ownership and routing without printing secrets."""
import json
from pathlib import Path
import subprocess

ids = subprocess.check_output(['docker', 'ps', '-aq'], text=True).split()
containers = json.loads(subprocess.check_output(['docker', 'inspect', *ids], text=True)) if ids else []
report = []
for item in containers:
    labels = item['Config'].get('Labels') or {}
    report.append({'name': item['Name'].lstrip('/'), 'id': item['Id'],
        'image': item['Config']['Image'], 'state': item['State']['Status'],
        'health': item['State'].get('Health', {}).get('Status'),
        'project': labels.get('com.docker.compose.project'),
        'service': labels.get('com.docker.compose.service'),
        'compose_files': labels.get('com.docker.compose.project.config_files'),
        'restart': item['HostConfig']['RestartPolicy'],
        'mounts': [{'type': m['Type'], 'source': m['Source'], 'destination': m['Destination']} for m in item['Mounts']],
        'ports': item['NetworkSettings']['Ports']})
print(json.dumps(report, indent=2))
