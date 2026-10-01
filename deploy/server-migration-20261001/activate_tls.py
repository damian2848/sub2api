from pathlib import Path
import subprocess

path = Path('/etc/nginx/sites-available/gpt56-migration.conf')
text = path.read_text()
text = text.replace('/opt/sub2api-gpt56/source-artifacts/tls/', '/etc/letsencrypt/live/gpt56-migration/')
text = text.replace('client_max_body_size 128m;', 'client_max_body_size 256m;\n    underscores_in_headers on;')
text = text.replace('proxy_set_header Host $host;', 'proxy_set_header Host sub2api.gpt56.site;\n        proxy_set_header X-Forwarded-Host $host;')
path.write_text(text)
subprocess.run(['nginx', '-t'], check=True)
subprocess.run(['systemctl', 'reload', 'nginx'], check=True)
Path('/etc/systemd/system/gpt56-tls-renew.service').write_text('''[Unit]
Description=Renew Gpt56 domain certificates
After=docker.service nginx.service network-online.target
[Service]
Type=oneshot
ExecStart=/usr/bin/docker run --rm --network host -v /etc/letsencrypt:/etc/letsencrypt -v /var/lib/letsencrypt:/var/lib/letsencrypt -v /var/log/letsencrypt:/var/log/letsencrypt certbot/certbot@sha256:09c5c6e899adc7d0c422c73ad181a6747cfe7399b2ecd8f61095ccb5740ef202 renew --cert-name gpt56-migration --quiet
ExecStartPost=/usr/sbin/nginx -t
ExecStartPost=/usr/bin/systemctl reload nginx
''')
Path('/etc/systemd/system/gpt56-tls-renew.timer').write_text('''[Unit]
Description=Check Gpt56 certificates twice daily
[Timer]
OnCalendar=*-*-* 03,15:00:00
RandomizedDelaySec=3600
Persistent=true
[Install]
WantedBy=timers.target
''')
subprocess.run(['systemctl', 'daemon-reload'], check=True)
subprocess.run(['systemctl', 'enable', '--now', 'gpt56-tls-renew.timer'], check=True)
print('All three domain certificates activated; renewal timer enabled')
