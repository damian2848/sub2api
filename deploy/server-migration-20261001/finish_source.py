"""Keep cached DNS clients working and retire the old writers and replication."""
import json
from pathlib import Path
import subprocess

root = Path('/root/sub2api-backups/server-migration-20261001')
check = subprocess.run(['curl', '-fsS', '--resolve', 'sub2api.gpt56.site:443:23.132.132.111',
                        '-D', '-', 'https://sub2api.gpt56.site/health'], capture_output=True, check=True)
assert b'target-production' in check.stdout and b'"status":"ok"' in check.stdout
old = (root / 'nginx-before-cutover.conf').read_text()
marker = 'server {\n    listen 443 ssl http2;\n    server_name sub2api.gpt56.site;'
assert marker in old
relay = '''server {
    listen 443 ssl http2;
    server_name sub2api.gpt56.site;
    ssl_certificate /etc/letsencrypt/live/sub2api.gpt56.site/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/sub2api.gpt56.site/privkey.pem;
    client_max_body_size 256m;
    large_client_header_buffers 4 16k;
    location / {
        proxy_pass https://23.132.132.111;
        proxy_ssl_server_name on;
        proxy_ssl_name sub2api.gpt56.site;
        proxy_ssl_verify on;
        proxy_ssl_verify_depth 5;
        proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 1800s;
        proxy_send_timeout 1800s;
        add_header X-Migration-Old-Entry relay-to-new-server always;
    }
}
'''
path = Path('/etc/nginx/sites-enabled/ai-services.conf').resolve()
path.write_text(old[:old.index(marker)] + relay)
subprocess.run(['nginx', '-t'], check=True)
subprocess.run(['systemctl', 'reload', 'nginx'], check=True)
sql = """SET default_transaction_read_only=off;
SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name='gpt56_migration_20261001';
ALTER ROLE gpt56_migration_repl NOLOGIN;
"""
subprocess.run(['docker', 'exec', '-i', 'sub2api-postgres', 'sh', '-c',
                'psql -X -q -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1'],
                input=sql, text=True, capture_output=True, check=True)
subprocess.run(['docker', 'update', '--restart=no', 'sub2api-redis'], check=True, stdout=subprocess.DEVNULL)
subprocess.run(['docker', 'stop', '-t', '30', 'sub2api-redis'], check=True, stdout=subprocess.DEVNULL)
keys = Path('/root/.ssh/authorized_keys')
lines = keys.read_text().splitlines()
lines = [x for x in lines if not x.endswith((' gpt56-migration-tunnel', ' gpt56-migration-data-sync', ' gpt56-migration-artifact-sync'))]
keys.write_text('\n'.join(lines) + '\n')
keys.chmod(0o600)
print(json.dumps({'old_domain_entry': 'relay-to-new-server', 'old_application_writers': 'stopped',
                  'old_postgres': 'read-only archive', 'old_redis': 'stopped archive',
                  'replication_slot_removed': True, 'migration_login_disabled': True}))
