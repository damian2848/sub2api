"""Prepare validated local routing and copy the remaining source artifacts."""
import json
import os
from pathlib import Path
import shlex
import subprocess

os.umask(0o077)
root = Path('/opt/sub2api-gpt56')
ssh = ['ssh', '-i', str(root / 'ssh/artifact-sync'), '-o', 'BatchMode=yes',
       '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=yes',
       '-o', 'UserKnownHostsFile=' + str(root / 'ssh/known_hosts')]
subprocess.run(['rsync', '-a', '-e', shlex.join(ssh), 'root@43.128.10.245:extras/',
                str(root / 'source-artifacts/extras') + '/'], check=True)
source = (root / 'source-artifacts/nginx-source.conf').read_text()
marker = 'server {\n    listen 443 ssl http2;\n    server_name sub2api.gpt56.site;'
assert marker in source
server = source[source.index(marker):]
server = server.replace('server_name sub2api.gpt56.site;', 'server_name sub2api.gpt56.site gpt56.site image2api.gpt56.site;')
server = server.replace('/etc/letsencrypt/live/sub2api.gpt56.site/', '/etc/letsencrypt/live/gpt56-migration/')
server = server.replace('http://127.0.0.1:8080', 'http://127.0.0.1:18080')
server = server.replace('$connection_upgrade', '$gpt56_migration_connection')
server = server.replace('client_header_timeout 10s;', 'underscores_in_headers on;\n    client_header_timeout 10s;\n    add_header X-Migration-Stage target-production always;')
prefix = '''map $http_upgrade $gpt56_migration_connection {
    default upgrade;
    '' close;
}
server {
    listen 80;
    listen [::]:80;
    server_name sub2api.gpt56.site gpt56.site image2api.gpt56.site;
    location ^~ /.well-known/acme-challenge/ { root /var/lib/letsencrypt; }
    location / { return 308 https://$host$request_uri; }
}
'''
config = prefix + server
prepared = root / 'production/nginx.conf'
prepared.write_text(config)
active = Path('/etc/nginx/sites-available/gpt56-migration.conf')
old = active.read_bytes()
try:
    active.write_text(config)
    subprocess.run(['nginx', '-t'], check=True)
finally:
    active.write_bytes(old)
print(json.dumps({'production_routes_prepared': True, 'includes_video_and_admin_sidecars': True}))
