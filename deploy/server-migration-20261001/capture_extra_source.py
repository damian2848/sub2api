"""Preserve proxy configuration and TLS dependencies for a later cutover."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

os.umask(0o077)
root = Path('/root/sub2api-backups/server-migration-20261001/extras')
root.mkdir(mode=0o700, exist_ok=True)
result = subprocess.run(['nginx', '-T'], capture_output=True, check=True)
(root / 'nginx-complete.conf').write_bytes(result.stdout)
text = result.stdout.decode(errors='replace')
tls = {}
for name in set(re.findall(r'\bssl_certificate(?:_key)?\s+([^;]+);', text)):
    original = Path(name)
    if original.is_file():
        dest = root / 'tls' / original.relative_to('/')
        dest.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        dest.write_bytes(original.read_bytes())
        dest.chmod(0o600)
        tls[name] = str(dest.relative_to(root))
(root / 'tls-paths.json').write_text(json.dumps(tls))
for name, directory in (('grok-video', '/opt/grok-video-adapter'), ('video-console', '/home/ubuntu/async-video-console')):
    for file in ('docker-compose.yml', '.env', 'Dockerfile', 'package.json', 'package-lock.json'):
        original = Path(directory) / file
        if original.is_file():
            dest = root / name / file
            dest.parent.mkdir(exist_ok=True, mode=0o700)
            dest.write_bytes(original.read_bytes())
            dest.chmod(0o600)
manifest = {}
for file in root.rglob('*'):
    if file.is_file() and file.name != 'SHA256.json':
        with file.open('rb') as stream:
            manifest[str(file.relative_to(root))] = hashlib.file_digest(stream, 'sha256').hexdigest()
(root / 'SHA256.json').write_text(json.dumps(manifest))
print(json.dumps({'tls_files_preserved': len(tls), 'dependency_files_preserved': len(manifest)}))
