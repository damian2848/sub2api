from pathlib import Path
import subprocess

Path('/etc/systemd/system/gpt56-backup.service').write_text('''[Unit]
Description=Back up Gpt56 production database and configuration
After=docker.service
[Service]
Type=oneshot
UMask=0077
Nice=15
ExecStart=/usr/bin/python3 /opt/sub2api-gpt56/backup_production.py
''')
Path('/etc/systemd/system/gpt56-backup.timer').write_text('''[Unit]
Description=Back up Gpt56 production daily
[Timer]
OnCalendar=*-*-* 19:30:00 UTC
RandomizedDelaySec=600
Persistent=true
[Install]
WantedBy=timers.target
''')
subprocess.run(['systemctl', 'daemon-reload'], check=True)
subprocess.run(['systemctl', 'enable', '--now', 'gpt56-backup.timer'], check=True)
subprocess.run(['systemctl', 'start', '--no-block', 'gpt56-backup.service'], check=True)
print('Daily backups enabled; first production backup started')
