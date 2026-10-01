from pathlib import Path
import subprocess

unit = '''[Unit]
Description=Encrypted temporary bridge to existing Gpt56 site
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/bin/ssh -NT -i /opt/sub2api-gpt56/ssh/tunnel -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/opt/sub2api-gpt56/ssh/known_hosts -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -L 127.0.0.1:28443:127.0.0.1:443 root@43.128.10.245
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
'''
Path('/etc/systemd/system/gpt56-bridge-tunnel.service').write_text(unit)
subprocess.run(['systemctl','daemon-reload'],check=True)
subprocess.run(['systemctl','enable','--now','gpt56-bridge-tunnel.service'],check=True)
file = Path('/etc/nginx/sites-available/gpt56-migration.conf')
text = file.read_text().replace('proxy_pass https://43.128.10.245;', 'proxy_pass https://127.0.0.1:28443;')
# SSH authenticates the source host and encrypts the complete localhost hop.
text = text.replace('proxy_ssl_verify on;', 'proxy_ssl_verify off;')
text = text.replace('return 308 https://$host$request_uri;', 'location ^~ /.well-known/acme-challenge/ { root /var/lib/letsencrypt; }\n    location / { return 308 https://$host$request_uri; }')
Path('/var/lib/letsencrypt/.well-known/acme-challenge').mkdir(parents=True, exist_ok=True)
file.write_text(text)
subprocess.run(['nginx','-t'],check=True)
subprocess.run(['systemctl','reload','nginx'],check=True)
print('Temporary HTTPS bridge now uses the authenticated SSH tunnel')
