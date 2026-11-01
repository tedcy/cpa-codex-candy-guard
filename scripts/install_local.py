import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

SOURCE = Path(__file__).resolve().parents[1]
WORKSPACE = SOURCE.parent
sys.path.insert(0, str(WORKSPACE / "upgrade-20261012/python-deps"))
import yaml

LOCAL = WORKSPACE / "local-cpa"
OUT = SOURCE / "test-output"
STAGE = OUT / "cpa-stage"
STAGE_NAME = "cpa-candy-guard-stage"
ID = "cpa-codex-candy-guard"
VERSION = "0.2.0"
BINARY = SOURCE / "dist" / (ID + "-v" + VERSION + ".so")
BASE = "http://127.0.0.1:18317"
STAGE_BASE = "http://127.0.0.1:28318"
KEY = next(line.split("=", 1)[1].strip().strip("\"'") for line in Path("D:/apps/cpa-usage-keeper/.env.docker").read_text(encoding="utf-8-sig").splitlines() if line.startswith("CPA_MANAGEMENT_KEY="))
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
if "\n" in KEY or "\r" in KEY:
    raise RuntimeError("Management key cannot contain newlines")


def emit(value):
    print(json.dumps(value, ensure_ascii=True), flush=True)


def run(args, check=True):
    result = subprocess.run([str(x) for x in args], capture_output=True, encoding="utf-8", errors="replace", timeout=90)
    if check and result.returncode:
        raise RuntimeError("Command failed: " + str(args[0]) + " " + str(args[1]))
    return result


def api(base, path, body=None, method="GET", key=KEY):
    headers = {"Authorization": "Bearer " + key}
    data = None
    if body is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(body).encode("utf-8")
    request = urllib.request.Request(base + path, headers=headers, data=data, method=method)
    with OPENER.open(request, timeout=35) as response:
        raw = response.read()
        try:
            return response.status, json.loads(raw)
        except ValueError:
            return response.status, raw


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def inspect(name):
    data = json.loads(run(["docker", "inspect", name]).stdout)[0]
    return {"id": data["Id"], "image": data["Config"]["Image"], "status": data["State"]["Status"], "health": data["State"].get("Health", {}).get("Status"), "restart_count": data["RestartCount"]}


def plugins(base):
    return [{"id": item.get("id"), "version": (item.get("metadata") or {}).get("version"), "enabled": item.get("effective_enabled"), "registered": item.get("registered")} for item in api(base, "/v8/management/plugins")[1].get("plugins", [])]


def verify(base):
    ps = plugins(base)
    guard = next(item for item in ps if item["id"] == ID)
    if not guard["enabled"] or not guard["registered"] or guard["version"] != VERSION:
        raise RuntimeError("Guard plugin registration failed")
    _, state = api(base, "/v0/management/plugins/" + ID + "/state")
    if state.get("error") or not state.get("ready") or state["settings"]["schedule_enabled"] or state["running"]:
        raise RuntimeError("Guard must be healthy and idle with its schedule off")
    if "recheck_enabled" not in state["settings"] or state["settings"]["recheck_delay_minutes"] < 1:
        raise RuntimeError("Second-check settings are missing or invalid")
    _, page = api(base, "/v0/resource/plugins/" + ID + "/ui")
    if not isinstance(page, bytes) or "二次检查".encode("utf-8") not in page or ("v" + VERSION).encode("ascii") not in page:
        raise RuntimeError("Plugin UI did not render")
    return {"plugins": ps, "guard_accounts": len(state["credentials"]), "guard_models": len(state["models"]), "schedule_enabled": state["settings"]["schedule_enabled"], "recheck_enabled": state["settings"]["recheck_enabled"], "round_count": len(state.get("rounds", [])), "ui_bytes": len(page), "binary_sha256": digest(BINARY)}


def wait_ready(base):
    for _ in range(30):
        try:
            return verify(base)
        except Exception:
            time.sleep(1)
    raise RuntimeError("CPA/Guard verification failed")


def stage():
    OUT.mkdir(exist_ok=True)
    if STAGE.exists():
        raise RuntimeError("Stage directory already exists")
    if run(["docker", "inspect", STAGE_NAME], check=False).returncode == 0:
        raise RuntimeError("Stage container already exists")
    config = yaml.safe_load((LOCAL / "config.yaml").read_text(encoding="utf-8-sig"))
    config["api-keys"] = {}
    config.setdefault("oauth", {})["auth-dir"] = "/root/.cli-proxy-api"
    config.setdefault("server", {})["host"] = "0.0.0.0"
    config["plugins"] = {"enabled": True, "dir": "plugins", "configs": {"cpa-codex-candy-eval": {"enabled": True}, ID: {"enabled": True}}}
    STAGE.mkdir()
    for name in ["auths", "plugins", "logs", "static"]:
        (STAGE / name).mkdir()
    binary_dir = STAGE / "plugins/linux/amd64"
    binary_dir.mkdir(parents=True)
    candy = next((LOCAL / "plugins/linux/amd64").glob("cpa-codex-candy-eval-v0.4.4.so"))
    shutil.copy2(candy, binary_dir / candy.name)
    shutil.copy2(BINARY, binary_dir / BINARY.name)
    (STAGE / "config.yaml").write_text(yaml.safe_dump(config, allow_unicode=True), encoding="utf-8")
    (STAGE / "guard.env").write_text("CPA_CANDY_GUARD_MANAGEMENT_KEY=" + KEY + "\n", encoding="utf-8")
    args = ["docker", "run", "--detach", "--pull=never", "--name", STAGE_NAME, "--env-file", STAGE / "guard.env", "--publish", "127.0.0.1:28318:8317"]
    for name, target in [("config.yaml", "/CLIProxyAPI/config.yaml"), ("auths", "/root/.cli-proxy-api"), ("plugins", "/CLIProxyAPI/plugins"), ("logs", "/CLIProxyAPI/logs"), ("static", "/CLIProxyAPI/static")]:
        args += ["--volume", str(STAGE / name) + ":" + target]
    try:
        run(args + [inspect("cli-proxy-api")["image"], "/CLIProxyAPI/CLIProxyAPI", "-local-model"])
        result = wait_ready(STAGE_BASE)
        settings = {"model": "gpt-6-astra", "per_credential_concurrency": 3, "credential_concurrency": 2, "interval_minutes": 10, "schedule_enabled": False, "recheck_delay_minutes": 10, "scope": "all_enabled", "credential_ids": []}
        for enabled in [False, True]:
            settings["recheck_enabled"] = enabled
            api(STAGE_BASE, "/v0/management/plugins/" + ID + "/settings", settings, "PUT")
            api(STAGE_BASE, "/v0/management/plugins/" + ID + "/run", {}, "POST")
            for _ in range(20):
                _, data = api(STAGE_BASE, "/v0/management/plugins/" + ID + "/state")
                if not data["running"]:
                    break
                time.sleep(0.1)
            if data["rounds"][-1]["items"] != [] or data["running"]:
                raise RuntimeError("Empty-account stage made unexpected work")
        result["empty_account_run"] = "completed_without_requests"
        result["empty_account_recheck"] = "completed_without_requests"
        (OUT / "stage-verification.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
        emit(result)
    finally:
        log = run(["docker", "logs", STAGE_NAME], check=False)
        (OUT / "stage.log").write_text(log.stdout + log.stderr, encoding="utf-8")
        run(["docker", "rm", "--force", STAGE_NAME], check=False)
        if STAGE.resolve().parent == OUT.resolve():
            shutil.rmtree(STAGE)


def prepare_config(raw):
    text = raw.decode("utf-8-sig")
    before = yaml.safe_load(text)
    if ID in before.get("plugins", {}).get("configs", {}):
        raise RuntimeError("Guard already configured; use the documented update procedure")
    lines = text.splitlines(keepends=True)
    start = next(i for i, line in enumerate(lines) if line.rstrip() == "plugins:")
    end = next((i for i in range(start + 1, len(lines)) if lines[i].strip() and not lines[i][0].isspace() and not lines[i].startswith("#")), len(lines))
    index = next(i for i in range(start, end) if lines[i].strip() == "configs:")
    indent = len(lines[index]) - len(lines[index].lstrip())
    child_indent = next((len(lines[i]) - len(lines[i].lstrip()) for i in range(index + 1, end) if lines[i].strip() and not lines[i].lstrip().startswith("#")), indent + 2)
    if child_indent <= indent:
        child_indent = indent + 2
    step = child_indent - indent
    lines.insert(index + 1, " " * child_indent + ID + ":\n" + " " * (child_indent + step) + "enabled: true\n")
    candidate = "".join(lines).encode("utf-8")
    before["plugins"]["configs"][ID] = {"enabled": True}
    if yaml.safe_load(candidate) != before:
        raise RuntimeError("Config update changed unrelated settings")
    return candidate


def install():
    if not (OUT / "stage-verification.json").is_file():
        raise RuntimeError("Stage verification must pass before installation")
    if json.loads((OUT / "stage-verification.json").read_text())["binary_sha256"] != digest(BINARY):
        raise RuntimeError("Binary changed after isolated verification; rerun stage")
    deadline = time.monotonic() + 600
    while True:
        _, state = api(BASE, "/v0/management/plugins/cpa-codex-candy-eval/state")
        active = sum(bool(c.get("running")) for c in state.get("credentials", []))
        if not active:
            break
        if time.monotonic() >= deadline:
            emit({"status": "deferred", "active_tests": active})
            return
        emit({"waiting_for_existing_tests": active})
        time.sleep(10)
    before = {"cpa": inspect("cli-proxy-api"), "keeper": inspect("cpa-usage-keeper"), "plugins": plugins(BASE)}
    backup = SOURCE / ("install-backup-" + datetime.datetime.now().strftime("%Y%m%d-%H%M%S"))
    backup.mkdir()
    for name in ["config.yaml", "docker-compose.yml"]:
        shutil.copy2(LOCAL / name, backup / name)
    shutil.copytree(LOCAL / "plugins", backup / "plugins")
    candidate = prepare_config((LOCAL / "config.yaml").read_bytes())
    original_compose = (LOCAL / "docker-compose.yml").read_bytes()
    parsed = yaml.safe_load(original_compose)
    service = parsed["services"]["cli-proxy-api"]
    if "env_file" in service:
        raise RuntimeError("Existing env_file needs explicit merge; refusing overwrite")
    compose_text = original_compose.decode("utf-8")
    newline = "\r\n" if "\r\n" in compose_text else "\n"
    marker = "    restart: unless-stopped" + newline
    if compose_text.count(marker) != 1:
        raise RuntimeError("Could not locate CPA service in Compose")
    new_compose = compose_text.replace(marker, marker + "    env_file:" + newline + "      - ./.candy-guard.env" + newline)
    expected = parsed
    expected["services"]["cli-proxy-api"]["env_file"] = ["./.candy-guard.env"]
    if yaml.safe_load(new_compose) != expected:
        raise RuntimeError("Compose change is not narrowly scoped")
    secret = LOCAL / ".candy-guard.env"
    if secret.exists():
        raise RuntimeError("Secret environment file already exists")
    target = LOCAL / "plugins/linux/amd64" / BINARY.name
    if target.exists():
        raise RuntimeError("Guard binary already exists")
    result = {"status": "starting", "backup": str(backup)}
    touched = False
    try:
        if (LOCAL / "config.yaml").read_bytes() != (backup / "config.yaml").read_bytes() or (LOCAL / "docker-compose.yml").read_bytes() != original_compose:
            raise RuntimeError("Production files changed after backup")
        run(["docker", "stop", "--time", "20", "cli-proxy-api"])
        touched = True
        secret.write_text("CPA_CANDY_GUARD_MANAGEMENT_KEY=" + json.dumps(KEY).replace("$", "$$") + "\n", encoding="utf-8")
        secret.chmod(0o600)
        shutil.copy2(BINARY, target)
        (LOCAL / "config.yaml").write_bytes(candidate)
        (LOCAL / "docker-compose.yml").write_bytes(new_compose.encode("utf-8"))
        run(["docker", "compose", "-f", LOCAL / "docker-compose.yml", "config", "--quiet"])
        run(["docker", "compose", "-f", LOCAL / "docker-compose.yml", "up", "-d", "--no-deps", "--force-recreate", "--pull", "never", "cli-proxy-api"])
        verification = wait_ready(BASE)
        new_plugins = {p["id"]: p for p in verification["plugins"]}
        for plugin in before["plugins"]:
            if new_plugins.get(plugin["id"]) != plugin:
                raise RuntimeError("Existing plugin version or state changed")
        if (LOCAL / "config.yaml").read_bytes() != candidate:
            raise RuntimeError("Configuration was rewritten unexpectedly")
        verification["keeper"] = inspect("cpa-usage-keeper")
        verification["cpa"] = inspect("cli-proxy-api")
        verification["only_guard_config_added"] = True
        verification["existing_plugins_unchanged"] = True
        result.update(status="success", verification=verification)
    except Exception as error:
        result.update(status="failed", error_type=type(error).__name__, error=str(error))
        if touched:
            run(["docker", "stop", "--time", "15", "cli-proxy-api"], check=False)
            for name in ["config.yaml", "docker-compose.yml"]:
                shutil.copy2(backup / name, LOCAL / name)
            if target.exists():
                target.unlink()
            if secret.exists():
                secret.unlink()
            run(["docker", "compose", "-f", LOCAL / "docker-compose.yml", "up", "-d", "--no-deps", "--force-recreate", "--pull", "never", "cli-proxy-api"])
            result["status"] = "rolled_back"
    finally:
        (OUT / "install-result.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
        (backup / "result.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
        emit(result)


def idle_guard(wait=False):
    deadline = time.monotonic() + (600 if wait else 0)
    while True:
        _, state = api(BASE, "/v0/management/plugins/" + ID + "/state")
        _, original = api(BASE, "/v0/management/plugins/cpa-codex-candy-eval/state")
        if state["settings"]["schedule_enabled"]:
            raise RuntimeError("Disable the guard schedule before updating")
        active = sum(bool(c.get("running")) for c in original["credentials"])
        if not state["running"] and active == 0:
            return state
        if time.monotonic() >= deadline:
            raise RuntimeError("Existing tests are still running; update postponed")
        emit({"waiting_for_existing_tests": active, "guard_running": state["running"]})
        time.sleep(10)


def update_binary():
    expected = json.loads((OUT / "stage-verification.json").read_text())
    if expected["binary_sha256"] != digest(BINARY):
        raise RuntimeError("Updated binary must pass stage verification")
    state = idle_guard(wait=True)
    original_plugins = plugins(BASE)
    keeper = inspect("cpa-usage-keeper")
    cpa = inspect("cli-proxy-api")
    plugin_root = (LOCAL / "plugins").resolve()
    binary_dir = plugin_root / "linux/amd64"
    old_binaries = list(binary_dir.glob(ID + "*.so"))
    target = binary_dir / BINARY.name
    pending = target.with_suffix(".pending")
    state_path = plugin_root / (ID + "-state.json")
    if not plugin_root.is_relative_to(LOCAL.resolve()) or any(not p.resolve().is_relative_to(plugin_root) for p in [binary_dir, target, pending, state_path, *old_binaries]):
        raise RuntimeError("Plugin paths must stay inside the local CPA plugin directory")
    if len(old_binaries) != 1 or target.exists() or pending.exists():
        raise RuntimeError("Expected one previous guard library and no new-version library")
    backup = SOURCE / ("install-backup-" + datetime.datetime.now().strftime("%Y%m%d-%H%M%S") + "-binary")
    backup.mkdir()
    for name in ["config.yaml", "docker-compose.yml"]:
        shutil.copy2(LOCAL / name, backup / name)
    shutil.copytree(plugin_root, backup / "plugins")
    config_hash = digest(LOCAL / "config.yaml")
    compose_hash = digest(LOCAL / "docker-compose.yml")
    result = {"status": "starting", "backup": str(backup)}
    touched = False
    try:
        latest = idle_guard()
        if latest["settings"] != state["settings"] or len(latest["rounds"]) != len(state["rounds"]):
            raise RuntimeError("Guard settings or history changed during preparation")
        run(["docker", "stop", "--time", "20", "cli-proxy-api"])
        touched = True
        if state_path.exists():
            shutil.copy2(state_path, backup / "plugins" / state_path.name)
        shutil.copy2(BINARY, pending)
        pending.replace(target)
        for old in old_binaries:
            old.unlink()
        run(["docker", "start", "cli-proxy-api"])
        verification = wait_ready(BASE)
        if digest(LOCAL / "config.yaml") != config_hash or digest(LOCAL / "docker-compose.yml") != compose_hash:
            raise RuntimeError("Configuration changed during binary update")
        current_plugins = {p["id"]: p for p in verification["plugins"]}
        for old in original_plugins:
            if old["id"] != ID and current_plugins.get(old["id"]) != old:
                raise RuntimeError("An existing plugin changed")
        _, after = api(BASE, "/v0/management/plugins/" + ID + "/state")
        old_settings = dict(state["settings"])
        old_settings.setdefault("recheck_enabled", False)
        old_settings.setdefault("recheck_delay_minutes", 10)
        if after["settings"] != old_settings or [r["id"] for r in after["rounds"]] != [r["id"] for r in state["rounds"]]:
            raise RuntimeError("Guard settings or history were not preserved")
        if {c["id"]: c["disabled"] for c in after["credentials"]} != {c["id"]: c["disabled"] for c in state["credentials"]}:
            raise RuntimeError("Credential status changed during upgrade")
        verification["keeper"] = inspect("cpa-usage-keeper")
        verification["cpa"] = inspect("cli-proxy-api")
        if verification["keeper"]["id"] != keeper["id"] or verification["keeper"]["health"] != "healthy" or verification["cpa"]["id"] != cpa["id"]:
            raise RuntimeError("CPA container identity or Keeper health changed")
        verification.update(existing_plugins_unchanged=True, settings_history_preserved=True, credential_status_preserved=True, config_compose_unchanged=True)
        result.update(status="success", verification=verification)
    except Exception as error:
        result.update(status="failed", error_type=type(error).__name__)
        if touched:
            run(["docker", "stop", "--time", "15", "cli-proxy-api"], check=False)
            if target.exists():
                target.unlink()
            for old in old_binaries:
                shutil.copy2(backup / "plugins/linux/amd64" / old.name, old)
            old_state = backup / "plugins" / state_path.name
            if old_state.exists():
                shutil.copy2(old_state, state_path)
            elif state_path.exists():
                state_path.unlink()
            run(["docker", "start", "cli-proxy-api"])
            for _ in range(30):
                try:
                    if plugins(BASE) == original_plugins:
                        result["status"] = "rolled_back"
                        break
                except Exception:
                    pass
                time.sleep(1)
    (OUT / "binary-update-result.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
    (backup / "result.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
    emit(result)
    if result["status"] != "success":
        raise RuntimeError("Guard update did not complete; inspect the private update result")


parser = argparse.ArgumentParser()
parser.add_argument("action", choices=["stage", "install", "verify", "update-binary"])
action = parser.parse_args().action
if action == "stage":
    stage()
elif action == "install":
    install()
elif action == "update-binary":
    update_binary()
else:
    emit(verify(BASE))
