#!/usr/bin/env python3
"""使用未修改的 CPA 与合成凭证验证插件模型查询名称，不访问真实账号。"""

import argparse
import hashlib
import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


ALIASES = {
    "gpt-6-astra-basispoints": "gpt-6-astra",
    "gpt-5.6-sol-basispoints": "gpt-5.6-sol",
}
API_KEY = "local-fixture-api-key"
MANAGEMENT_KEY = "local-fixture-management-key"


def encode(value):
    return json.dumps(value, separators=(",", ":")).encode()


class Upstream(BaseHTTPRequestHandler):
    records = []

    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        account = self.headers.get("ChatGPT-Account-ID")
        assert account in {"fixture-account-a", "fixture-account-b"}
        assert self.headers.get("Authorization") == "Bearer fixture-token-" + account[-1]
        assert body["model"] in ALIASES.values()
        self.records.append({"account": account, "model": body["model"]})
        response = {
            "id": "resp_fixture",
            "object": "response",
            "status": "completed",
            "model": body["model"],
            "output": [{
                "id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": account, "annotations": []}],
            }],
            "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
        }
        payload = encode(response)
        if body.get("stream"):
            payload = b"data: " + encode({"type": "response.completed", "response": response}) + b"\n\n"
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if body.get("stream") else "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", required=True, type=Path)
    parser.add_argument("--plugin", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    host, plugin = args.host.resolve(), args.plugin.resolve()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    run = Path(tempfile.mkdtemp(prefix="auth-model-names-", dir=output))
    for name in ("auths", "plugins"):
        (run / name).mkdir()
    shutil.copy2(plugin, run / "plugins" / ("oai-basispoints" + plugin.suffix))
    source_data = {}
    for account in ("a", "b"):
        name = "account-" + account + ".json"
        source_data[name] = {
            "type": "codex", "access_token": "fixture-token-" + account,
            "refresh_token": "fixture-refresh-" + account, "account_id": "fixture-account-" + account,
            "plan_type": "pro", "expired": "2100-01-01T00:00:00Z",
            "last_refresh": datetime.now(timezone.utc).isoformat(),
            "websockets": False, "disabled": False,
        }
        (run / "auths" / name).write_bytes(encode(source_data[name]))

    upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    base = "http://127.0.0.1:" + str(port)
    process, log = None, None
    # 本机测试不继承用户的 HTTP 代理，避免把夹具请求发往外部代理。
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(path, method="GET", payload=None, management=True):
        req = urllib.request.Request(
            base + path, method=method, data=None if payload is None else encode(payload),
            headers={"Authorization": "Bearer " + (MANAGEMENT_KEY if management else API_KEY),
                     "Content-Type": "application/json"},
        )
        try:
            response = client.open(req, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.read(), {key.lower(): value for key, value in response.headers.items()}

    def wait_for(description, predicate):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            assert process.poll() is None, "CPA exited; inspect " + str(run / "cpa.log")
            try:
                if predicate():
                    return
            except (OSError, json.JSONDecodeError):
                pass
            time.sleep(0.1)
        raise AssertionError("Timed out: " + description)

    def auths():
        status, raw, _ = request("/v0/management/auth-files")
        assert status == 200, status
        return json.loads(raw)["files"]

    def models(name):
        status, raw, _ = request("/v0/management/auth-files/models?" + urllib.parse.urlencode({"name": name}))
        assert status == 200, status
        return [item["id"] for item in json.loads(raw)["models"]]

    def stop():
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        if log is not None:
            log.close()

    def start(enabled):
        nonlocal process, log
        config = f"""host: 127.0.0.1
port: {port}
auth-dir: {json.dumps(str(run / 'auths'))}
api-keys: [{API_KEY}]
remote-management:
  secret-key: {MANAGEMENT_KEY}
  allow-remote: false
  disable-control-panel: true
request-retry: 0
max-retry-credentials: 1
plugins:
  enabled: {str(enabled).lower()}
  dir: {json.dumps(str(run / 'plugins'))}
  configs:
    oai-basispoints:
      enabled: true
      data_dir: ""
      upstream_transport: http
      responses_url: http://127.0.0.1:{upstream.server_port}/responses
      models: {json.dumps(list(ALIASES))}
      model_mappings: {json.dumps(ALIASES)}
"""
        (run / "config.yaml").write_text(config)
        log = (run / "cpa.log").open("a")
        # 不继承生产存储、代理或管理密钥环境，宿主只读写本次临时目录。
        process = subprocess.Popen([str(host), "--config", str(run / "config.yaml"), "--local-model"],
                                   cwd=run, stdout=log, stderr=subprocess.STDOUT,
                                   env={"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(run)})
        wait_for("auth registration", lambda: len(auths()) == (4 if enabled else 2))

    def identities():
        return {row["id"]: (row["name"], row["provider"], row["auth_index"]) for row in auths()}

    def set_disabled(name, disabled):
        status, _, _ = request("/v0/management/auth-files/status", "PATCH", {"name": name, "disabled": disabled})
        assert status == 200, status
        wait_for("source status sync", lambda: all(row["disabled"] == disabled for row in auths()
                                                   if Path(row["path"]).name == name))

    try:
        start(False)
        original_models = {name: models(name) for name in source_data}
        assert all(original_models.values()), "native baseline model lists are empty"
        stop()
        start(True)
        status, _, headers = request("/v0/management/auth-files")
        assert status == 200
        expected_names = set(source_data) | {"oai-basispoints/bp-account-a", "oai-basispoints/bp-account-b"}
        initial_identities = identities()
        assert {row[0] for row in initial_identities.values()} == expected_names
        for _ in range(12):
            for name in source_data:
                assert models(name) == original_models[name], "native model list changed"
                virtual_id = "bp-" + Path(name).stem
                assert models("oai-basispoints/" + virtual_id) == list(ALIASES)
                assert models(virtual_id) == list(ALIASES)
        for name, original in source_data.items():
            assert (run / "auths" / name).read_bytes() == encode(original), "startup rewrote source OAuth"

        # 虚拟记录只能展示和查询，不能被当成源文件独立开关。
        status, _, _ = request("/v0/management/auth-files/status", "PATCH",
                               {"name": "oai-basispoints/bp-account-a", "disabled": True})
        assert status == 409, "virtual entry was incorrectly accepted as a source file"

        for account, other in (("a", "b"), ("b", "a")):
            set_disabled("account-" + other + ".json", True)
            set_disabled("account-" + account + ".json", False)
            wait_for("disabled model removal", lambda: models("oai-basispoints/bp-account-" + other) == [])
            for alias, canonical in ALIASES.items():
                for stream in (False, True):
                    before = len(Upstream.records)
                    status, raw, _ = request("/v1/responses", "POST", {
                        "model": alias, "stream": stream,
                        "input": [{"role": "user", "content": "local identity fixture"}],
                    }, management=False)
                    assert status == 200, (status, raw[:200])
                    expected_account = "fixture-account-" + account
                    assert expected_account.encode() in raw
                    assert Upstream.records[before:] == [{"account": expected_account, "model": canonical}]
            set_disabled("account-" + other + ".json", False)

        # 源认证编辑入口仍只返回两份物理文件，热重载不改变四条内存身份。
        status, raw, _ = request("/v0/management/oai-basispoints/source-auths")
        assert status == 200
        sources = json.loads(raw)["files"]
        assert {item["name"] for item in sources} == set(source_data)
        first = next(item for item in sources if item["name"] == "account-a.json")
        for enabled in (True, False):
            status, _, _ = request("/v0/management/oai-basispoints/source-auths", "PATCH",
                                   {"auth_index": first["auth_index"], "websockets": enabled})
            assert status == 200
            wait_for("source reload", lambda: all(row.get("websockets", False) == enabled for row in auths()
                                                 if Path(row["path"]).name == "account-a.json"))
            assert identities() == initial_identities
            assert models("account-a.json") == original_models["account-a.json"]
            assert models("oai-basispoints/bp-account-a") == list(ALIASES)

        assert sorted(path.name for path in (run / "auths").rglob("*.json")) == sorted(source_data)
        for name, original in source_data.items():
            assert json.loads((run / "auths" / name).read_bytes()) == original
        report = {
            "host_version": headers.get("x-cpa-version"), "host_commit": headers.get("x-cpa-commit"),
            "host_sha256": hashlib.sha256(host.read_bytes()).hexdigest(),
            "plugin_sha256": hashlib.sha256(plugin.read_bytes()).hexdigest(),
            "host_modified": False, "physical_credentials": 2, "runtime_records": 4,
            "native_models_preserved": True, "unique_lookup_names": True,
            "virtual_write_rejected": True, "hot_reload_identity_preserved": True,
            "synthetic_generation_requests": len(Upstream.records), "real_upstream_tested": False,
        }
        (run / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(report, indent=2))
        print("Report:", run / "report.json")
    finally:
        stop()
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    main()
