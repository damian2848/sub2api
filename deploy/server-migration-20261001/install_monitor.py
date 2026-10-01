import subprocess
from pathlib import Path

root = Path('/opt/sub2api-gpt56')
unit = '''[Unit]
Description=Check Gpt56 production application, PostgreSQL and Redis
After=docker.service nginx.service

[Service]
Type=oneshot
UMask=0077
ExecStart=/usr/bin/python3 /opt/sub2api-gpt56/verify_target.py --monitor
'''
timer = '''[Unit]
Description=Verify Gpt56 production every minute

[Timer]
OnBootSec=90s
OnUnitInactiveSec=60s
Persistent=true

[Install]
WantedBy=timers.target
'''
Path('/etc/systemd/system/gpt56-sync-monitor.service').write_text(unit)
Path('/etc/systemd/system/gpt56-sync-monitor.timer').write_text(timer)
subprocess.run(['systemctl','daemon-reload'], check=True)
subprocess.run(['systemctl','start','gpt56-sync-monitor.service'], check=True)
subprocess.run(['systemctl','enable','--now','gpt56-sync-monitor.timer'], check=True)
print('Production monitor is enabled')
