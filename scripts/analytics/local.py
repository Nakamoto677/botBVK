#!/usr/bin/env python3
"""Local Metabase pilot against an existing Docker PostgreSQL bot database.

Setup creates only analytics objects, dedicated roles and a Metabase application
database. Bot source rows are read-only; no Telegram bot process is started.
Runtime credentials, logs and PIDs are under ignored data/metabase-local (0700).
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import sys
import time
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[2]
RUNTIME = ROOT / "data" / "metabase-local"
URL = "http://localhost:3000"


def protected(path, content):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(content)
    os.chmod(path, 0o600)


def run(args, *, env=None, data=None, timeout=180):
    result = subprocess.run(args, cwd=ROOT, env=env, input=data, text=True,
                            capture_output=True, timeout=timeout)
    if result.returncode:
        protected(RUNTIME / "command-error.log", result.stderr + result.stdout)
        raise RuntimeError("Command failed; details in private data/metabase-local/command-error.log")
    return result.stdout


def state():
    return json.loads((RUNTIME / "secrets.json").read_text())


def save(values):
    protected(RUNTIME / "secrets.json", json.dumps(values, indent=2) + "\n")


def source_sql(values, sql):
    return run(["docker", "exec", "-i", values["source_container"], "psql", "-X",
                "-v", "ON_ERROR_STOP=1", "-U", values["source_admin"], "-d",
                values["source_database"], "-At"], data=sql)


def compose(values, *args):
    env = os.environ.copy()
    env["BVK_SOURCE_NETWORK"] = values["source_network"]
    if values.get("metabase_image"):
        env["METABASE_IMAGE"] = values["metabase_image"]
    return run(["docker", "compose", "-p", "bvk-analytics-local", "-f",
                "compose.analytics.local.yml", *args], env=env, timeout=300)


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


def is_analytics(record):
    if not alive(record["pid"]):
        return False
    command = subprocess.run(["ps", "-p", str(record["pid"]), "-o", "command="],
                             capture_output=True, text=True).stdout
    return command.strip().startswith(record["command"] + " ")


def stop_analytics():
    marker = RUNTIME / "analytics-process.json"
    if not marker.exists():
        return
    record = json.loads(marker.read_text())
    command = subprocess.run(["ps", "-p", str(record["pid"]), "-o", "command="],
                             capture_output=True, text=True).stdout
    if command.strip().startswith(record["command"] + " "):
        os.kill(record["pid"], signal.SIGTERM)
        for _ in range(50):
            if not alive(record["pid"]):
                break
            time.sleep(0.1)
    marker.unlink()


def start(values):
    compose(values, "up", "-d")
    marker = RUNTIME / "analytics-process.json"
    if marker.exists() and is_analytics(json.loads(marker.read_text())):
        print("Analytics process already running", flush=True)
        return
    env = os.environ.copy()
    env["DATABASE_URL"] = values["writer_url"]
    env.pop("TELEGRAM_BOT_TOKEN", None)
    with (RUNTIME / "analytics.log").open("ab") as log:
        process = subprocess.Popen([str(ROOT / "bin" / "bvk-analytics"), "-config",
                                    str(ROOT / "config.json")], cwd=ROOT, env=env,
                                   stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                                   start_new_session=True)
    protected(marker, json.dumps({"pid": process.pid,
                                  "command": str(ROOT / "bin" / "bvk-analytics")}) + "\n")
    time.sleep(1)
    if process.poll() is not None:
        raise RuntimeError("Analytics stopped; see private analytics.log")
    print("Metabase and minute analytics started", flush=True)


def api(method, path, body=None, token=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["X-Metabase-Session"] = token
    req = Request(URL + "/api/" + path, method=method, headers=headers,
                  data=None if body is None else json.dumps(body).encode())
    try:
        with urlopen(req, timeout=120) as response:
            raw = response.read()
            return json.loads(raw) if raw else None
    except HTTPError as error:
        protected(RUNTIME / "api-error.json", error.read().decode(errors="replace"))
        raise RuntimeError(f"Metabase {method} /api/{path}: HTTP {error.code}; private api-error.json") from None


def wait_ready():
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        try:
            if api("GET", "health").get("status") == "ok":
                print("Metabase ready", flush=True)
                return
        except (OSError, RuntimeError, ValueError):
            pass
        time.sleep(2)
    raise RuntimeError("Metabase startup timed out; inspect docker logs bvk-metabase-local")


def refresh_ui(values):
    """Back up managed presentation objects, then update without changing permissions."""
    token = api("POST", "session", {"username": values["admin"]["email"],
                                   "password": values["admin"]["password"]})["id"]
    imported = json.loads((RUNTIME / "import-state.json").read_text())
    backup = {"cards": {}, "dashboards": {}, "homepage": {}}
    for kind in ("cards", "dashboards"):
        endpoint = "card" if kind == "cards" else "dashboard"
        for key, ident in imported[kind].items():
            backup[kind][key] = api("GET", f"{endpoint}/{ident}", token=token)
    for setting in api("GET", "setting", token=token):
        if setting["key"] in ("custom-homepage", "custom-homepage-dashboard"):
            backup["homepage"][setting["key"]] = setting.get("value")
    protected(RUNTIME / f"ui-before-{time.time_ns()}.json", json.dumps(backup))
    run([sys.executable, "analytics/metabase/build.py"])
    env = os.environ.copy()
    env["METABASE_SESSION"] = token
    env.pop("METABASE_API_KEY", None)
    print(run([sys.executable, "analytics/metabase/import.py", "--url", URL,
               "--database-id", str(values["database_id"]), "--state",
               str(RUNTIME / "import-state.json"), "--validate-queries", "--set-homepage"],
              env=env, timeout=600), end="", flush=True)


def configure_managers(values, token):
    groups = api("GET", "permissions/group", token=token)
    manager_group = next((g for g in groups if g["name"] == "Руководители BVK"), None)
    if manager_group is None:
        manager_group = api("POST", "permissions/group", {"name": "Руководители BVK"}, token)
    group_id = manager_group["id"]
    all_users = next(g["id"] for g in groups if g["name"] == "All Users")
    for account in values["managers"]:
        if not account.get("id"):
            user = api("POST", "user", {"email": account["email"], "password": account["password"],
                       "first_name": account["first_name"], "last_name": "BVK"}, token)
            account["id"] = user["id"]
            save(values)
            api("POST", "permissions/membership", {"user_id": user["id"], "group_id": group_id}, token)
    imported = json.loads((RUNTIME / "import-state.json").read_text())
    collection_id = str(imported["collection_id"])
    graph = api("GET", "collection/graph", token=token)
    # All Users must not re-grant edits to our view-only group.
    graph["groups"][str(all_users)] = {"root": "none", collection_id: "none"}
    graph["groups"][str(group_id)] = {"root": "none", collection_id: "read"}
    api("PUT", "collection/graph", graph, token)
    graph = api("GET", "permissions/graph", token=token)
    db_id = str(values["database_id"])
    for group in (all_users, group_id):
        permissions = graph["groups"].setdefault(str(group), {}).setdefault(db_id, {})
        permissions["view-data"] = "unrestricted"
        permissions["create-queries"] = "no"
        permissions["download"] = {"schemas": "full"}
    api("PUT", "permissions/graph", graph, token)
    url = URL + "/dashboard/" + str(imported["dashboards"]["support"]) + "#refresh=60"
    lines = ["# Локальная панель BVK", "", "Адрес: " + url, "",
             "Файл содержит новые локальные пароли. Не добавлять его в Git.", "",
             "| Аккаунт | Email | Пароль |", "|---|---|---|",
             f"| Администратор | {values['admin']['email']} | {values['admin']['password']} |"]
    for account in values["managers"]:
        lines.append(f"| {account['first_name']} (просмотр) | {account['email']} | {account['password']} |")
    protected(RUNTIME / "ACCESS.md", "\n".join(lines) + "\n")
    print("Two view-only manager accounts ready; access details in private ACCESS.md", flush=True)


def setup(source_container):
    RUNTIME.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(RUNTIME, 0o700)
    if (RUNTIME / "secrets.json").exists():
        values = state()
        if values["source_container"] != source_container:
            raise RuntimeError("Existing pilot belongs to a different source container")
    else:
        inspect = json.loads(run(["docker", "inspect", source_container]))[0]
        source_env = dict(item.split("=", 1) for item in inspect["Config"]["Env"] if "=" in item)
        networks = [name for name in inspect["NetworkSettings"]["Networks"] if name != "bridge"]
        if not networks:
            raise RuntimeError("Source needs an existing user-defined Docker network")
        ports = inspect["NetworkSettings"]["Ports"].get("5432/tcp") or []
        if not ports:
            raise RuntimeError("Source PostgreSQL needs a local published port")
        password = lambda: secrets.token_urlsafe(24) + "Aa1!"
        values = {"source_container": source_container, "source_network": networks[0],
                  "source_admin": source_env.get("POSTGRES_USER", "postgres"),
                  "source_database": source_env.get("POSTGRES_DB", "postgres"),
                  "source_port": int(ports[0]["HostPort"]),
                  "source_password": source_env.get("POSTGRES_PASSWORD", ""),
                  "writer_password": password(), "reader_password": password(),
                  "app_password": password(), "encryption_key": secrets.token_hex(32),
                  "admin": {"email": "admin@bvk.local", "password": password()},
                  "managers": [{"email": f"manager{i}@bvk.local", "password": password(),
                                "first_name": f"Руководитель {i}"} for i in (1, 2)]}
        db = quote(values["source_database"], safe="")
        suffix = f"@127.0.0.1:{values['source_port']}/{db}?sslmode=disable"
        values["writer_url"] = "postgres://bvk_analytics_writer:" + quote(values["writer_password"], safe="") + suffix
        values["admin_url"] = "postgres://" + quote(values["source_admin"], safe="") + ":" + quote(values["source_password"], safe="") + suffix
        collision = source_sql(values, "SELECT rolname FROM pg_roles WHERE rolname IN ('bvk_analytics_writer','bvk_metabase_reader','bvk_metabase_app'); SELECT datname FROM pg_database WHERE datname='bvk_metabase';")
        if collision.strip():
            raise RuntimeError("Metabase roles/database already exist; refusing to replace their credentials")
        save(values)
    if not values.get("database_ready"):
        # Retain a source backup before additive schema/role installation.
        backup = RUNTIME / "bot-before-analytics.dump"
        with backup.open("wb") as out:
            result = subprocess.run(["docker", "exec", source_container, "pg_dump", "-U",
                                     values["source_admin"], "-d", values["source_database"], "-Fc"],
                                    stdout=out, stderr=subprocess.PIPE)
        os.chmod(backup, 0o600)
        if result.returncode:
            raise RuntimeError("Source backup failed")
        env = os.environ.copy()
        env["GOCACHE"] = "/private/tmp/bvk-analytics-go-cache"
        run(["go", "build", "-trimpath", "-o", "bin/bvk-analytics", "./cmd/bvk-analytics"], env=env)
        env["DATABASE_URL"] = values["admin_url"]
        run([str(ROOT / "bin" / "bvk-analytics"), "-config", "config.json", "-migrate"], env=env)
        provision = (ROOT / "scripts" / "analytics" / "provision.sql").read_text()
        run(["docker", "exec", "-i", source_container, "psql", "-X", "-v", "ON_ERROR_STOP=1",
             "-v", "source_db=" + values["source_database"], "-U", values["source_admin"],
             "-d", values["source_database"]], data=provision)
        password_sql = []
        for role, field in (("bvk_analytics_writer", "writer_password"),
                            ("bvk_metabase_reader", "reader_password"), ("bvk_metabase_app", "app_password")):
            literal = values[field].replace("'", "''")
            password_sql.append(f"ALTER ROLE {role} PASSWORD '{literal}';")
        source_sql(values, "\n".join(password_sql))
        source_sql(values, (ROOT / "scripts" / "analytics" / "check-privileges.sql").read_text())
        values["database_ready"] = True
        save(values)
        print("Source backup retained; analytics roles and app database created", flush=True)
    protected(RUNTIME / "metabase.env", "\n".join([
        "MB_DB_HOST=" + values["source_container"], "MB_DB_PORT=5432", "MB_DB_DBNAME=bvk_metabase",
        "MB_DB_USER=bvk_metabase_app", "MB_DB_PASS=" + values["app_password"],
        "MB_ENCRYPTION_SECRET_KEY=" + values["encryption_key"]]) + "\n")
    images = json.loads(run(["docker", "image", "inspect", "metabase/metabase:v0.64.1.x"]))
    values["metabase_image"] = images[0]["RepoDigests"][0]
    save(values)
    start(values)
    wait_ready()
    properties = api("GET", "session/properties")
    if properties.get("setup-token"):
        result = api("POST", "setup", {"token": properties["setup-token"],
                     "prefs": {"site_name": "Поддержка BVK — локальная панель", "site_locale": "ru", "allow_tracking": False},
                     "user": {"email": values["admin"]["email"], "password": values["admin"]["password"],
                              "first_name": "Администратор", "last_name": "BVK", "site_name": "Поддержка BVK"}})
        token = result["id"]
    else:
        token = api("POST", "session", {"username": values["admin"]["email"], "password": values["admin"]["password"]})["id"]
    for name, value in (("site-locale", "ru"), ("report-timezone", "Asia/Irkutsk"),
                        ("enable-public-sharing", False), ("enable-embedding", False),
                        ("anon-tracking-enabled", False)):
        api("PUT", "setting/" + name, {"value": value}, token)
    if not values.get("database_id"):
        db = api("POST", "database", {"name": "BVK — аналитика", "engine": "postgres",
                 "details": {"host": values["source_container"], "port": 5432,
                             "dbname": values["source_database"], "user": "bvk_metabase_reader",
                             "password": values["reader_password"], "ssl": False,
                             "schema-filters-type": "inclusion", "schema-filters-patterns": "analytics"},
                 "is_full_sync": True}, token)
        values["database_id"] = db["id"]
        save(values)
    env = os.environ.copy()
    env["METABASE_SESSION"] = token
    env.pop("METABASE_API_KEY", None)
    output = run([sys.executable, "analytics/metabase/import.py", "--url", URL, "--database-id",
                  str(values["database_id"]), "--state", str(RUNTIME / "import-state.json"),
                  "--validate-queries"], env=env, timeout=600)
    print(output, end="", flush=True)
    values["imported"] = True
    save(values)
    configure_managers(values, token)
    print("Local panel ready: " + URL + "/dashboard/1#refresh=60", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("setup", "start", "stop", "status", "restart-analytics", "refresh-ui"))
    parser.add_argument("--source-container", default="bot-bvk-postgres-1")
    args = parser.parse_args()
    if args.action == "setup":
        setup(args.source_container)
        return
    values = state()
    if args.action == "refresh-ui":
        refresh_ui(values)
    elif args.action == "start":
        start(values)
    elif args.action == "restart-analytics":
        stop_analytics()
        start(values)
    elif args.action == "stop":
        stop_analytics()
        compose(values, "stop")
        print("Stopped analytics and Metabase; source PostgreSQL/bot untouched")
    else:
        print(compose(values, "ps"), end="")
        try:
            print("Metabase:", api("GET", "health").get("status"))
        except (OSError, RuntimeError):
            print("Metabase: unavailable")
        marker = RUNTIME / "analytics-process.json"
        print("Analytics:", "running" if marker.exists() and is_analytics(json.loads(marker.read_text())) else "stopped")


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
