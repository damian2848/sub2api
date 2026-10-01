from pathlib import Path
import subprocess

path = Path('/etc/nginx/sites-enabled/ai-services.conf').resolve()
text = path.read_text()
assert 'proxy_pass https://23.132.132.111;' in text
text = text.replace('proxy_ssl_verify on;', 'proxy_ssl_verify on;\n        proxy_ssl_verify_depth 5;')
path.write_text(text)
subprocess.run(['nginx', '-t'], check=True)
subprocess.run(['systemctl', 'reload', 'nginx'], check=True)
print('Old entrypoint certificate chain verification depth updated')
