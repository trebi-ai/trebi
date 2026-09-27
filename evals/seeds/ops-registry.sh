#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
# A fixture MCP registry on a free local port, so `connector install` reads a
# known catalog instead of the network.
PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
REG="$EVAL_WORKDIR/registry"
mkdir -p "$REG"
cat > "$REG/list.json" <<'JSON'
{
  "servers": [
    {"server": {
      "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
      "name": "acme/echo",
      "description": "Echo the input back.",
      "version": "1.0.0",
      "packages": [{
        "registryType": "npm",
        "identifier": "acme-echo-mcp",
        "version": "1.0.0",
        "runtimeHint": "npx",
        "transport": {"type": "stdio"},
        "environmentVariables": [
          {"name": "ECHO_TOKEN", "description": "API token", "format": "string", "isRequired": true, "isSecret": true}
        ]
      }]
    }, "_meta": {}}
  ],
  "metadata": {"nextCursor": "", "count": 1}
}
JSON
python3 - "$REG" <<'PY'
import json, sys
reg = sys.argv[1]
doc = json.load(open(f"{reg}/list.json"))
entry = {"server": doc["servers"][0]["server"], "_meta": {}}
json.dump(entry, open(f"{reg}/acme_echo-1.0.0.json", "w"))
PY
cat > "$REG/server.py" <<'PY'
import http.server, json, os, sys, urllib.parse

root, port = sys.argv[2], int(sys.argv[1])

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        path = urllib.parse.unquote(self.path.split("?")[0])
        if path.rstrip("/") == "/v0.1/servers":
            name = "list.json"
        else:
            rest = path[len("/v0.1/servers/"):]
            stem = rest.replace("/versions/", "-").replace("/", "_")
            name = stem + ".json"
            if stem.endswith("-latest"):
                # One version in this fixture: any "latest" read maps to it.
                prefix = stem[: -len("-latest")]
                matches = sorted(f for f in os.listdir(root) if f.startswith(prefix + "-"))
                if len(matches) != 1:
                    self.send_error(404)
                    return
                name = matches[0]
        try:
            body = json.load(open(os.path.join(root, name)))
        except OSError:
            self.send_error(404)
            return
        out = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY
nohup python3 "$REG/server.py" "$PORT" "$REG" > "$EVAL_WORKDIR/registry.log" 2>&1 &
# The trial teardown sends SIGTERM to each <workdir>/*.pid.
echo $! > "$EVAL_WORKDIR/registry.pid"
for _ in $(seq 1 40); do
  curl -fsS "http://127.0.0.1:$PORT/v0.1/servers" > /dev/null 2>&1 && break
  sleep 0.25
done
trebi config set connectors.registries "[{\"name\":\"fixture\",\"url\":\"http://127.0.0.1:$PORT\"}]"
trebi agent create reviewer --harness codex --prompt "You review changes. Do what the job says."
