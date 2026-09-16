#!/usr/bin/env node
// Records real Claude Code stream-json NDJSON sessions into
// ../../internal/runtime/cli/testdata/claude/<sdk>-<cli>/.
//
// Dev/CI only: needs the operator's `claude` on PATH and a shared login. The
// script never reads or copies tokens; it spawns the CLI exactly like the Go
// driver does and answers can_use_tool over the control channel.
//
// Usage:
//   npm ci
//   node generate.mjs            # all scenarios
//   node generate.mjs basic      # one scenario
//
// The recorder sanitizes cwd/session ids and trims the giant init tool list;
// everything else is byte-for-byte CLI output.

import { spawn, execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { randomUUID } from "node:crypto";
import { homedir } from "node:os";

const here = dirname(fileURLToPath(import.meta.url));
const home = homedir();
const outRoot = join(here, "..", "..", "internal", "runtime", "cli", "testdata", "claude");

const sdkVersion = JSON.parse(
  execFileSync("node", ["-p", "JSON.stringify(require('./package.json'))"], { cwd: here }).toString()
).dependencies["@anthropic-ai/claude-agent-sdk"];
const cliVersion = execFileSync("claude", ["--version"]).toString().trim().split(" ")[0];

const target = process.argv[2] || "all";
const dir = join(outRoot, `${sdkVersion}-${cliVersion}`);
const work = join(here, ".work");
rmSync(work, { recursive: true, force: true });
mkdirSync(work, { recursive: true });

function sanitize(line, realCwd) {
  let text = line.trim();
  if (!text) return null;
  let m;
  try {
    m = JSON.parse(text);
  } catch {
    return null;
  }
  if (m.type === "system" && m.subtype === "init") {
    m = { type: "system", subtype: "init", session_id: m.session_id, model: m.model, permissionMode: m.permissionMode, cwd: "/tmp/ws" };
  }
  m.session_id = m.session_id ? "11111111-2222-3333-4444-555555555555" : m.session_id;
  // Replace the recorder's real work dir in the serialized frame as well: it
  // appears inside tool inputs and wire_tool_inputs, not just top-level cwd.
  // Streaming `input_json_delta` frames split the path at arbitrary bytes, so
  // scrub the home prefix too — a partial `"/Users/name/Pro|jects..."` chunk
  // still becomes a harmless `/tmp/Pro|jects...`.
  const out = JSON.stringify(m)
    .split(realCwd).join("/tmp/ws")
    .split(home).join("/tmp");
  return out.length > 4 << 20 ? null : out;
}

class Recorder {
  constructor(argv) {
    this.argv = argv;
    this.lines = [];
    this.waiters = [];
    this.proc = null;
    this.result = null;
    this.sawResult = false;
  }

  start() {
    this.proc = spawn(this.argv[0], this.argv.slice(1), {
      stdio: ["pipe", "pipe", "pipe"],
    });
    let buf = "";
    this.proc.stdout.on("data", (chunk) => {
      buf += chunk.toString();
      let i;
      while ((i = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, i);
        buf = buf.slice(i + 1);
        let m;
        try {
          m = JSON.parse(line);
        } catch {
          continue;
        }
        this.lines.push(line);
        if (m.type === "result") this.sawResult = true;
        if (m.type === "control_request" && m.request && m.request.subtype === "can_use_tool") {
          this.onPermission(m);
        }
        for (const w of [...this.waiters]) {
          if (w.match(m)) {
            this.waiters.splice(this.waiters.indexOf(w), 1);
            w.resolve(m);
          }
        }
      }
    });
    return this;
  }

  onPermission(m) {
    // Default: allow. Scenarios override.
  }

  send(obj) {
    this.proc.stdin.write(JSON.stringify(obj) + "\n");
  }

  prompt(text) {
    this.send({
      type: "user",
      session_id: "",
      message: { role: "user", content: text },
      parent_tool_use_id: null,
    });
  }

  waitFor(match, timeoutMs = 180000) {
    const existing = this.lines.map((l) => JSON.parse(l)).find(match);
    if (existing) return Promise.resolve(existing);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error("timeout")), timeoutMs);
      this.waiters.push({
        match,
        resolve: (m) => {
          clearTimeout(timer);
          resolve(m);
        },
      });
    });
  }

  async close() {
    this.proc.stdin.end();
    await new Promise((resolve) => {
      const t = setTimeout(() => {
        this.proc.kill("SIGKILL");
        resolve();
      }, 5000);
      this.proc.on("exit", () => {
        clearTimeout(t);
        resolve();
      });
    });
  }
}

function write(name, rec) {
  mkdirSync(dir, { recursive: true });
  const body = rec.lines.map((l) => sanitize(l, work)).filter(Boolean).join("\n") + "\n";
  writeFileSync(join(dir, `${name}.ndjson`), body);
  console.log(`wrote ${join(dir, `${name}.ndjson`)} (${rec.lines.length} frames)`);
}

function baseArgs(extra) {
  return [
    "claude",
    "--output-format", "stream-json",
    "--verbose",
    "--include-partial-messages",
    ...extra,
    "--permission-prompt-tool", "stdio",
    "--permission-mode", "default",
    "--model", "haiku",
    "--max-turns", "4",
  ];
}

async function scenarioBasic() {
  const rec = new Recorder(baseArgs(["-p", "--input-format", "stream-json", "--session-id", randomUUID()])).start();
  rec.prompt("Reply with exactly PONG and nothing else.");
  await rec.waitFor((m) => m.type === "result");
  await rec.close();
  write("basic", rec);
}

async function scenarioPermission({ allow }) {
  const rec = new Recorder(baseArgs(["--input-format", "stream-json", "--session-id", randomUUID()])).start();
  rec.onPermission = (m) => {
    rec.send({
      type: "control_response",
      response: {
        subtype: "success",
        request_id: m.request_id,
        response: allow
          ? { behavior: "allow", updatedInput: m.request.input }
          : { behavior: "deny", message: "denied by fixture recorder" },
      },
    });
  };
  const target2 = join(work, `fixture-${allow ? "allow" : "deny"}.txt`);
  rec.prompt(`Use the Write tool to create the file ${target2} with content hello, then reply DONE.`);
  await rec.waitFor((m) => m.type === "result");
  await rec.close();
  write(allow ? "permission_allow" : "permission_deny", rec);
}

const scenarios = {
  basic: scenarioBasic,
  permission_allow: () => scenarioPermission({ allow: true }),
  permission_deny: () => scenarioPermission({ allow: false }),
};

for (const [name, fn] of Object.entries(scenarios)) {
  if (target !== "all" && target !== name) continue;
  console.log(`running scenario ${name}...`);
  await fn();
}
writeFileSync(
  join(dir, "meta.json"),
  JSON.stringify({ sdk: sdkVersion, cli: cliVersion, recorded_at: new Date().toISOString() }, null, 2) + "\n"
);
console.log(`fixtures: ${dir}`);
