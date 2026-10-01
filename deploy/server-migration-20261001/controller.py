"""Orchestrate migration over authenticated SSH, keeping secrets out of output."""
import json
import os
from pathlib import Path
import subprocess
import sys

HERE = Path(__file__).resolve().parent
TARGET = ['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15', 'us-server']
SOURCE = ['ssh', '-J', 'us-server', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15', 'sub2api-prod']

def remote(command, script, payload, timeout=3600):
    data = 'PAYLOAD = ' + repr(payload) + '\n' + (HERE / script).read_text()
    process = subprocess.run(command + ['python3 -u -'], input=data, text=True, capture_output=True, timeout=timeout)
    if process.returncode:
        private = HERE / ('error-' + script + '.log')
        private.write_text(process.stderr)
        private.chmod(0o600)
        raise RuntimeError('Remote phase failed; private diagnostic: ' + str(private))
    return process.stdout

if sys.argv[1] == 'initialize':
    host_keys = subprocess.check_output(['ssh-keygen', '-F', '43.128.10.245'], text=True)
    result = json.loads(remote(TARGET, 'bootstrap_target.py', {'known_hosts': host_keys}))
    print(json.dumps({'phase': 'target-private-staging-created'}), flush=True)
    output = remote(SOURCE, 'prepare_source.py', result)
    print(output, end='', flush=True)
elif sys.argv[1] == 'target':
    output = remote(TARGET, 'configure_target.py', {})
    print(output, end='', flush=True)
else:
    raise RuntimeError('Unknown migration phase')
