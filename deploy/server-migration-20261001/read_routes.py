import re
import subprocess

text = subprocess.check_output(['nginx','-T'], stderr=subprocess.DEVNULL, text=True)
for line in text.splitlines():
    if re.search(r'authorization|x-api-key|password|secret|token', line, re.I):
        print('    # credential-bearing directive omitted')
    else:
        print(re.sub(r'(https?://)[^/\s@]+@', r'\1[redacted]@', line))
