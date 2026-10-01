"""Serve migrated DNS safely through the still-active source during data copy."""
import json
from pathlib import Path
import subprocess

root = Path('/opt/sub2api-gpt56')
cert = root / 'source-artifacts/tls/fullchain.pem'
key = root / 'source-artifacts/tls/privkey.pem'
details = subprocess.check_output(['openssl', 'x509', '-in', str(cert), '-noout', '-ext', 'subjectAltName', '-dates'], text=True)
assert 'gpt56.site' in details
config = '''map $http_upgrade $gpt56_migration_connection {
    default upgrade;
    '' close;
}

server {
    listen 80;
    listen [::]:80;
    server_name sub2api.gpt56.site gpt56.site image2api.gpt56.site;
    return 308 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name sub2api.gpt56.site gpt56.site image2api.gpt56.site;
    ssl_certificate /opt/sub2api-gpt56/source-artifacts/tls/fullchain.pem;
    ssl_certificate_key /opt/sub2api-gpt56/source-artifacts/tls/privkey.pem;
    client_max_body_size 128m;
    client_body_timeout 3600s;
    location / {
        proxy_pass https://43.128.10.245;
        proxy_ssl_server_name on;
        proxy_ssl_name sub2api.gpt56.site;
        proxy_ssl_verify on;
        proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $gpt56_migration_connection;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        add_header X-Migration-Stage source-bridge always;
    }
}
'''
file = Path('/etc/nginx/sites-available/gpt56-migration.conf')
assert not file.exists(), 'Migration route already exists; inspect before replacing'
file.write_text(config)
file.chmod(0o644)
link = Path('/etc/nginx/sites-enabled/gpt56-migration.conf')
link.symlink_to(file)
result = subprocess.run(['nginx','-t'], capture_output=True)
if result.returncode:
    link.unlink()
    raise RuntimeError('Bridge Nginx configuration failed validation')
subprocess.run(['systemctl','reload','nginx'], check=True)
print(json.dumps({'temporary_bridge_installed':True,'certificate':details.strip(),'domains':['sub2api.gpt56.site','gpt56.site','image2api.gpt56.site']}))
