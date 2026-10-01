"""Remove the root-domain app alias, preserving ACME and production routes."""
import datetime
import json
import os
from pathlib import Path
import subprocess

os.umask(0o077)
root = Path('/opt/sub2api-gpt56')
active = Path('/etc/nginx/sites-available/gpt56-migration.conf')
backup = root / 'cleanup' / datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
backup.mkdir(parents=True, mode=0o700)
original = active.read_text()
(backup / 'nginx-before.conf').write_text(original)
names = 'server_name sub2api.gpt56.site gpt56.site image2api.gpt56.site;'
assert names in original, 'Inspect changed domain routing before modifying it'
updated = original.replace(names, 'server_name sub2api.gpt56.site image2api.gpt56.site;')
redirect = '''
server {
    listen 80;
    listen [::]:80;
    server_name gpt56.site;
    location ^~ /.well-known/acme-challenge/ { root /var/lib/letsencrypt; }
    location / { return 308 https://sub2api.gpt56.site$request_uri; }
}
server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name gpt56.site;
    ssl_certificate /etc/letsencrypt/live/gpt56-migration/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/gpt56-migration/privkey.pem;
    return 308 https://sub2api.gpt56.site$request_uri;
}
'''
active.write_text(updated + redirect)
result = subprocess.run(['nginx', '-t'], capture_output=True, text=True)
if result.returncode:
    active.write_text(original)
    raise RuntimeError('Nginx validation failed; original file restored')
subprocess.run(['systemctl', 'reload', 'nginx'], check=True)
(root / 'production/nginx.conf').write_text(updated + redirect)
print(json.dumps({'root_domain': 'redirect-only', 'destination': 'https://sub2api.gpt56.site$request_uri',
                  'configuration_backup': str(backup)}))
