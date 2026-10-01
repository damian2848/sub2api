"""Retain client identity through the old entrypoint and stop isolated rehearsal."""
from pathlib import Path
import subprocess

root = Path('/opt/sub2api-gpt56')
for path in (root / 'production/nginx.conf', Path('/etc/nginx/sites-available/gpt56-migration.conf')):
    text = path.read_text()
    if 'set_real_ip_from 43.128.10.245;' not in text:
        text = text.replace('client_header_timeout 10s;', '''set_real_ip_from 43.128.10.245;
    real_ip_header X-Forwarded-For;
    real_ip_recursive on;
    client_header_timeout 10s;''')
        path.write_text(text)
subprocess.run(['nginx', '-t'], check=True)
subprocess.run(['systemctl', 'reload', 'nginx'], check=True)
subprocess.run(['docker', 'compose', '-f', str(root / 'preview/compose.json'), 'stop'], check=True)
subprocess.run(['docker', 'update', '--restart=no', 'gpt56-preview-app', 'gpt56-preview-postgres', 'gpt56-preview-redis'], check=True, stdout=subprocess.DEVNULL)
print('Production proxy preserves original client IP; isolated preview is stopped')
