import json
from pathlib import Path
import subprocess

name = 'sub2api_sub2api-network'
network = json.loads(subprocess.check_output(['docker', 'network', 'inspect', name]))[0]
assert network['Labels']['com.docker.compose.project'] == 'sub2api'
assert not network['Containers'], 'Network still has connected containers'
subprocess.run(['docker', 'network', 'rm', network['Id']], check=True, stdout=subprocess.DEVNULL)
receipt = Path('/opt/sub2api-gpt56/cleanup/20261001T104237Z/receipt.json')
data = json.loads(receipt.read_text())
data['removed_empty_networks'].append(name)
receipt.write_text(json.dumps(data, indent=2))
print(json.dumps({'removed_empty_network': name}))
