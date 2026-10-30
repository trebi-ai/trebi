// trebi lifecycle plugin for OpenCode.
// Forwards session/permission events to `trebi agent-hook` when TREBI_RUN_ID is set.
// Materialized under ~/.trebi/harness/opencode/plugins/ (never user config).

export const TrebiLifecycle = async ({ $ }) => {
  const taskID = process.env.TREBI_RUN_ID || ""
  if (!taskID) {
    return {}
  }

  const mapEvent = (type) => {
    switch (type) {
      case "session.created":
        return "session-start"
      case "session.idle":
        return "stop"
      case "session.deleted":
        return "session-end"
      case "permission.asked":
        return "waiting"
      case "permission.replied":
        return "running"
      default:
        return ""
    }
  }

  const fire = async (eventName, extra = {}) => {
    if (!eventName) return
    const body = JSON.stringify({
      task_id: taskID,
      tool: "opencode",
      ...extra,
    })
    try {
      await $`sh -c ${`printf '%s' ${JSON.stringify(body)} | trebi agent-hook ${eventName}`}`
    } catch {
      // fail-soft: never break the OpenCode session
    }
  }

  return {
    event: async ({ event }) => {
      const type = event?.type || ""
      const name = mapEvent(type)
      const sessionID =
        event?.properties?.sessionID ||
        event?.properties?.sessionId ||
        event?.sessionID ||
        event?.sessionId ||
        ""
      const extra = {}
      if (sessionID) extra.session_id = sessionID
      await fire(name, extra)
    },
    "tool.execute.before": async () => {
      await fire("running")
    },
  }
}
