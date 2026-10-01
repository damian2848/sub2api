"""Inspect live dependencies without revealing credentials or changing services."""
import json
from pathlib import Path
import subprocess


def run(args, data=None, timeout=45):
    result = subprocess.run(args, input=data, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError("Read-only check failed: " + " ".join(args[:4]))
    return result.stdout.strip()


def sql(query):
    return run(["docker", "exec", "-i", "sub2api-postgres", "sh", "-c",
                'psql -X -q -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -At'], query)


db = json.loads(run(["docker", "inspect", "sub2api-postgres"]))[0]
app = json.loads(run(["docker", "inspect", "sub2api"]))[0]
redis = json.loads(run(["docker", "inspect", "sub2api-redis"]))[0]
env = dict(item.split("=", 1) for item in app["Config"]["Env"] if "=" in item)
result = {
    "postgres_version": sql("SELECT version();"),
    "postgres_settings": sql("SELECT name||'='||setting FROM pg_settings WHERE name IN ('wal_level','wal_keep_size','max_wal_senders','max_replication_slots','max_slot_wal_keep_size','data_directory');"),
    "replication_slots": sql("SELECT slot_name,slot_type,active,COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(),restart_lsn)::text,'') FROM pg_replication_slots;"),
    "replication_hba": sql("SELECT type,database,user_name,address,auth_method FROM pg_hba_file_rules WHERE error IS NULL;"),
    "database_names": sql("SELECT datname,pg_database_size(oid) FROM pg_database WHERE NOT datistemplate;"),
    "schema_migration_count": sql("SELECT count(*) FROM schema_migrations;"),
    "platform_accounts": sql("SELECT platform,count(*) FROM accounts WHERE deleted_at IS NULL GROUP BY platform;"),
    "proxy_endpoints": sql("SELECT id,name,protocol,host,port FROM proxies WHERE deleted_at IS NULL ORDER BY id;"),
    "docker_endpoints": {
        container["Name"].lstrip("/"): {name: data["IPAddress"] for name, data in container["NetworkSettings"]["Networks"].items()}
        for container in (db, app, redis)
    },
    "app_dependency_endpoints": {key: env[key] for key in (
        "DATABASE_HOST", "DATABASE_PORT", "DATABASE_DBNAME", "REDIS_HOST", "REDIS_PORT", "REDIS_DB",
        "GATEWAY_ANTIGRAVITY_FORWARD_BASE_URL", "TICKET_ENABLED", "TICKET_WORKERS", "TICKET_POLICY"
    ) if key in env},
    "data_files": run(["find", "/home/ubuntu/sub2api/deploy/data", "-maxdepth", "2", "-type", "f", "-printf", "%P %s bytes\n"]),
    "running_services": run(["systemctl", "list-units", "--type=service", "--state=running", "--no-pager"]),
    "rrsync": run(["sh", "-c", "command -v rrsync || true"]),
    "redis_version": run(["docker", "exec", "sub2api-redis", "redis-server", "--version"]),
    "redis_replication": run(["docker", "exec", "sub2api-redis", "redis-cli", "--no-auth-warning", "INFO", "replication"]),
    "source_version": run(["docker", "exec", "sub2api", "/app/sub2api", "-version"]),
}
print(json.dumps(result, indent=2))
