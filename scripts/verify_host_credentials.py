#!/usr/bin/env python3
"""在未修改的 CPA 上验收 host 模式；只使用临时目录和本地合成凭据。"""

import argparse
import base64
import hashlib
import http.client
import http.server
import json
import pathlib
import select
import shutil
import socket
import struct
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa", required=True, type=pathlib.Path)
    parser.add_argument("--plugin", required=True, type=pathlib.Path)
    args = parser.parse_args()
    cpa, plugin = args.cpa.resolve(strict=True), args.plugin.resolve(strict=True)
    root = pathlib.Path(tempfile.mkdtemp(prefix="basispoints-host-acceptance-"))
    print(f"evidence={root}", flush=True)
    (root / "auths").mkdir()
    (root / "plugins").mkdir()
    shutil.copy2(plugin, root / "plugins" / ("oai-basispoints" + plugin.suffix))
    records, proxy_counts = [], {"global": 0, "credential": 0}
    lock = threading.Lock()
    response = {"id": "resp_fixture", "object": "response", "status": "completed", "output": [
        {"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed",
         "content": [{"type": "output_text", "text": "HOST_ACCEPTANCE_OK", "annotations": []}]}],
        "usage": {"input_tokens": 2, "output_tokens": 3, "total_tokens": 5}}

    class QuietHandler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_):
            pass

        def reply(self, status, body, content_type="application/json"):
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    class Upstream(QuietHandler):
        def record(self, transport):
            with lock:
                records.append((transport, self.headers.get("Authorization")))

        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            self.record("http")
            event = json.dumps({"type": "response.completed", "response": response})
            self.reply(200, f"event: response.completed\ndata: {event}\n\n".encode(), "text/event-stream")

        def do_GET(self):
            if self.headers.get("Upgrade", "").lower() != "websocket":
                self.reply(404, b"{}")
                return
            key = self.headers["Sec-WebSocket-Key"]
            digest = hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", base64.b64encode(digest).decode())
            self.end_headers()
            self.wfile.flush()
            header = self.rfile.read(2)
            if len(header) != 2:
                return
            size = header[1] & 127
            if size == 126:
                size = struct.unpack("!H", self.rfile.read(2))[0]
            elif size == 127:
                size = struct.unpack("!Q", self.rfile.read(8))[0]
            mask = self.rfile.read(4) if header[1] & 128 else b""
            payload = self.rfile.read(size)
            if mask:
                payload = bytes(value ^ mask[i % 4] for i, value in enumerate(payload))
            assert json.loads(payload)["type"] == "response.create"
            self.record("ws")
            frame = json.dumps({"type": "response.completed", "response": response}).encode()
            prefix = bytes([129, len(frame)]) if len(frame) < 126 else bytes([129, 126]) + struct.pack("!H", len(frame))
            self.connection.sendall(prefix + frame)
            self.close_connection = True

    servers = []

    def start_server(handler):
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        servers.append(server)
        return server, f"http://127.0.0.1:{server.server_port}"

    upstream, upstream_url = start_server(Upstream)

    def make_proxy(label):
        class Proxy(QuietHandler):
            def count(self):
                with lock:
                    proxy_counts[label] += 1

            def do_CONNECT(self):
                # 夹具代理只准连接当前本地上游，避免后台请求访问互联网。
                if self.path != f"127.0.0.1:{upstream.server_port}":
                    self.reply(502, b"{}")
                    return
                self.count()
                with socket.create_connection(("127.0.0.1", upstream.server_port), timeout=5) as remote:
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.flush()
                    self.close_connection = True
                    peers = [self.connection, remote]
                    while True:
                        ready, _, _ = select.select(peers, [], [], 10)
                        if not ready:
                            return
                        for source in ready:
                            data = source.recv(65536)
                            if not data:
                                return
                            (remote if source is self.connection else self.connection).sendall(data)

            def do_POST(self):
                target = urllib.parse.urlsplit(self.path)
                if target.hostname != "127.0.0.1" or target.port != upstream.server_port:
                    self.reply(502, b"{}")
                    return
                self.count()
                data = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                conn = http.client.HTTPConnection("127.0.0.1", upstream.server_port, timeout=5)
                try:
                    conn.request("POST", target.path, data, dict(self.headers))
                    received = conn.getresponse()
                    self.reply(received.status, received.read(), received.getheader("Content-Type"))
                finally:
                    conn.close()
        return Proxy

    _, global_proxy = start_server(make_proxy("global"))
    _, account_proxy = start_server(make_proxy("credential"))
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    config = root / "config.yaml"
    config.write_text(f"""host: 127.0.0.1
port: {port}
auth-dir: {root / 'auths'}
api-keys: [fixture-api-key]
proxy-url: {global_proxy}
request-retry: 0
remote-management:
  secret-key: fixture-management-key
  disable-control-panel: true
plugins:
  enabled: true
  dir: {root / 'plugins'}
  configs:
    oai-basispoints:
      enabled: true
      data_dir: ""
      upstream_transport: auto
      responses_url: {upstream_url}/responses
""")
    auth_file = root / "auths" / "fixture.json"
    auth_file.write_text(json.dumps({"type": "codex", "access_token": "fixture-token-before", "account_id": "fixture-account",
                                    "expired": "2030-01-01T00:00:00Z", "websockets": False}))
    auth_file.chmod(0o600)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    process, log = None, None
    checks = []

    def call(path, data=None, method=None, management=False):
        key = "fixture-management-key" if management else "fixture-api-key"
        request = urllib.request.Request(f"http://127.0.0.1:{port}" + path,
            data=None if data is None else json.dumps(data).encode(), method=method,
            headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
        try:
            with opener.open(request, timeout=10) as result:
                return result.status, result.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def stop():
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        if log:
            log.close()

    def start():
        nonlocal process, log
        log = (root / "cpa.log").open("ab")
        process = subprocess.Popen([str(cpa), "-local-model", "-config", str(config)], cwd=root, stdout=log, stderr=log)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError(f"CPA exited; inspect {root / 'cpa.log'}")
            try:
                status, body = call("/v1/models")
                if status == 200 and b"gpt-6-astra-basispoints" in body:
                    return
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.1)
        raise RuntimeError(f"CPA startup timed out; inspect {root / 'cpa.log'}")

    def patch(**fields):
        status, body = call("/v0/management/auth-files/fields", {"name": "fixture.json", **fields}, "PATCH", True)
        assert status == 200, (status, body.decode()[:300])
        stored = json.loads(auth_file.read_text())
        for key, value in fields.items():
            assert stored.get(key, "") == value, f"native management failed to persist {key}"

    def generate(name, proxy, transport, token, stream=False):
        with lock:
            before = proxy_counts.copy()
            count = len(records)
        status, body = call("/v1/responses", {"model": "gpt-6-astra-basispoints", "input": "fixture", "stream": stream})
        assert status == 200 and b"HOST_ACCEPTANCE_OK" in body, (name, status, body.decode()[:500])
        with lock:
            assert records[count:] == [(transport, "Bearer " + token)], f"{name}: duplicate generation or stale credential"
            assert {key: proxy_counts[key] - before[key] for key in before} == {
                "global": int(proxy == "global"), "credential": int(proxy == "credential")}, f"{name}: proxy mismatch"
        checks.append(name)

    try:
        start()
        status, body = call("/v0/management/auth-files", management=True)
        assert status == 200
        entries = json.loads(body)["files"]
        assert len(entries) == 1 and entries[0]["provider"] == "codex", "native auth was expanded"
        checks.append("single_native_credential")
        status, _ = call("/v0/management/oai-basispoints/source-auths", management=True)
        assert status == 404, "removed management endpoint is still registered"
        checks.append("no_plugin_settings_endpoint")
        generate("global_proxy_http", "global", "http", "fixture-token-before")
        generate("global_proxy_sse", "global", "http", "fixture-token-before", stream=True)
        patch(proxy_url=account_proxy)
        generate("credential_proxy_http", "credential", "http", "fixture-token-before")
        patch(websockets=True)
        generate("native_ws_setting", "credential", "ws", "fixture-token-before")
        patch(proxy_url="")
        generate("global_proxy_ws", "global", "ws", "fixture-token-before")
        # 模拟宿主已持久化的新 access token，不声称完成了真实 OAuth 刷新。
        patch(access_token="fixture-token-after")
        generate("persisted_token_read", "global", "ws", "fixture-token-after")
        stop()
        start()
        generate("restart_reads_saved_token_and_ws", "global", "ws", "fixture-token-after")
        patch(websockets=False)
        generate("native_ws_disable", "global", "http", "fixture-token-after")
        result = {"status": "passed", "checks": checks, "proxy_counts": proxy_counts,
                  "limitations": ["synthetic OAuth data; no live refresh endpoint", "local upstream only"]}
        (root / "summary.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
        print(json.dumps(result, ensure_ascii=False, indent=2))
    finally:
        stop()
        for server in servers:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    main()
