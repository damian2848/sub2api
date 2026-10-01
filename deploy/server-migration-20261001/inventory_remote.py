"""Read-only migration inventory; never print environment secrets."""
import json
from pathlib import Path
import re
import shutil
import subprocess


def run(args, timeout=30):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    return {"code": result.returncode, "stdout": result.stdout.strip()}


report = {
    "hostname": run(["hostname"])["stdout"],
    "os": Path("/etc/os-release").read_text(),
    "cpu_count": run(["nproc"])["stdout"],
    "memory": run(["free", "-m"])["stdout"],
    "disk": run(["df", "-h", "/", "/var/lib/docker"])["stdout"],
    "listeners": run(["ss", "-lnt"])["stdout"],
    "tools": {name: shutil.which(name) for name in ("docker", "rsync", "nginx", "pg_dump", "psql", "wireguard", "wg")},
    "containers": [],
    "deploy_paths": [],
    "nginx_routes": [],
}
if shutil.which("docker"):
    report["docker_version"] = run(["docker", "version", "--format", "{{.Server.Version}}"])["stdout"]
    report["compose_version"] = run(["docker", "compose", "version", "--short"])["stdout"]
    ids = run(["docker", "ps", "-aq"])["stdout"].splitlines()
    if ids:
        inspected = json.loads(run(["docker", "inspect", *ids])["stdout"])
        for container in inspected:
            env = dict(value.split("=", 1) for value in container["Config"].get("Env", []) if "=" in value)
            labels = container["Config"].get("Labels") or {}
            report["containers"].append({
                "name": container["Name"].lstrip("/"),
                "image": container["Config"]["Image"],
                "image_id": container["Image"],
                "status": container["State"]["Status"],
                "health": container["State"].get("Health", {}).get("Status"),
                "network_mode": container["HostConfig"]["NetworkMode"],
                "ports": container["NetworkSettings"].get("Ports"),
                "mounts": [{key: item.get(key) for key in ("Type", "Name", "Source", "Destination", "RW")} for item in container.get("Mounts", [])],
                "compose_workdir": labels.get("com.docker.compose.project.working_dir"),
                "compose_files": labels.get("com.docker.compose.project.config_files"),
                "env_names": sorted(env),
                "runtime_role": env.get("RUNTIME_ROLE", "full (default)"),
            })
for directory in ("/home/ubuntu/sub2api/deploy", "/opt/sub2api", "/root/sub2api", "/etc/nginx/sites-enabled"):
    root = Path(directory)
    if root.exists():
        report["deploy_paths"].append({"directory": directory, "files": sorted(item.name for item in root.iterdir())})
for root in (Path("/etc/nginx/sites-enabled"), Path("/etc/nginx/conf.d")):
    if root.exists():
        for file in root.iterdir():
            if file.is_file():
                text = file.read_text(errors="replace")
                report["nginx_routes"].append({
                    "file": str(file),
                    "server_names": re.findall(r"\bserver_name\s+([^;]+);", text),
                    "listeners": re.findall(r"\blisten\s+([^;]+);", text),
                    "upstreams": re.findall(r"\bproxy_pass\s+(https?://[A-Za-z0-9.:[\]_-]+)", text),
                })
print(json.dumps(report, indent=2))
