// Trebi lifecycle extension for Pi coding agent.
// Forwards session/agent events to `trebi agent-hook` when TREBI_RUN_ID is set.
// Materialized under ~/.trebi/harness/pi/extensions/ (via PI_CODING_AGENT_DIR).

import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { spawn } from "node:child_process";

export default function (pi: ExtensionAPI) {
  const runID = process.env.TREBI_RUN_ID || "";
  if (!runID) {
    return;
  }

  const fire = (eventName: string, extra: Record<string, string> = {}) => {
    if (!eventName) return;
    const body = JSON.stringify({
      task_id: runID,
      tool: "pi",
      ...extra,
    });
    try {
      const child = spawn("trebi", ["agent-hook", eventName], {
        stdio: ["pipe", "ignore", "ignore"],
        detached: true,
        env: process.env,
      });
      child.stdin?.write(body);
      child.stdin?.end();
      child.unref();
    } catch {
      // fail-soft: never break the Pi session
    }
  };

  const sessionExtra = (ctx: ExtensionContext): Record<string, string> => {
    try {
      const sid =
        (ctx.sessionManager as { getSessionId?: () => string }).getSessionId?.() ||
        "";
      if (sid) return { session_id: sid };
    } catch {
      // ignore
    }
    return {};
  };

  pi.on("session_start", async (_event, ctx) => {
    fire("session-start", sessionExtra(ctx));
  });

  pi.on("before_agent_start", async () => {
    fire("running");
  });

  pi.on("agent_start", async () => {
    fire("running");
  });

  pi.on("turn_start", async () => {
    fire("running");
  });

  pi.on("tool_execution_start", async () => {
    fire("running");
  });

  // agent_settled: no auto-retry / follow-up left — maps to idle (stop).
  pi.on("agent_settled", async () => {
    fire("stop");
  });

  pi.on("session_shutdown", async () => {
    fire("session-end");
  });
}
