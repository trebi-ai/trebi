#!/usr/bin/env node
// Optional SDK conformance oracle (Part P): runs one prompt through the
// official TypeScript Agent SDK and asserts the same semantic events the Go
// driver produces (init + session id, assistant text, terminal result).
//
// Never a runtime dependency and never required for a Trebi run:
//   npm ci && node conformance.mjs
import { query } from "@anthropic-ai/claude-agent-sdk";
import { execFileSync } from "node:child_process";

execFileSync("claude", ["--version"]);

const seen = { init: false, assistant: false, result: false };
let sessionId = "";
let text = "";

for await (const message of query({
  prompt: "Reply with exactly PONG and nothing else.",
  options: {
    permissionMode: "default",
    model: "haiku",
    maxTurns: 1,
    includePartialMessages: true,
    env: { CLAUDE_CODE_ENTRYPOINT: "trebi-conformance" },
  },
})) {
  if (message.type === "system" && message.subtype === "init") {
    seen.init = true;
    sessionId = message.session_id ?? "";
  }
  if (message.type === "assistant") {
    for (const block of message.message?.content ?? []) {
      if (block.type === "text") text += block.text;
    }
  }
  if (message.type === "result") {
    seen.result = true;
    if (message.is_error) {
      console.error("SDK returned an error result:", message);
      process.exit(1);
    }
  }
}

if (!seen.init || !sessionId) throw new Error("no init/session id");
if (!seen.assistant) throw new Error("no assistant text");
if (!/pong/i.test(text)) throw new Error(`assistant text not PONG: ${text}`);
if (!seen.result) throw new Error("no result");
console.log("SDK conformance ok: init+session id, assistant text, success result");
