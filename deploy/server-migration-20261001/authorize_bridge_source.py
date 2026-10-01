from pathlib import Path

file = Path('/root/.ssh/authorized_keys')
text = file.read_text()
lines = text.splitlines()
matches = [i for i, line in enumerate(lines) if line.endswith(' gpt56-migration-tunnel')]
assert len(matches) == 1
i = matches[0]
if 'permitopen="127.0.0.1:443"' not in lines[i]:
    lines[i] = lines[i].replace(',command="/bin/false"', ',permitopen="127.0.0.1:443",command="/bin/false"')
    file.write_text('\n'.join(lines) + '\n')
    file.chmod(0o600)
print('Authenticated SSH forwarding to the source HTTPS listener is allowed')
