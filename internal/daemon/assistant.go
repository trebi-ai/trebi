package daemon

// Assistant channel glue (plan D.9, E): headless permission timeouts open an
// inbox item, and each finished turn mails one reply when the exchange began
// by email.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/store"
	"sort"
	"strings"
)

// OpenPermissionTimeout records the headless permission timeout: an inbox
// item that links to the conversation plus the permission.timeout log event
// (plan D.6, F.4).
func (c *ControlPlane) OpenPermissionTimeout(ctx context.Context, workspaceID, conversationID, tool string) error {
	id := ulid.Make().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	subject := "assistant/" + conversationID
	title := "Assistant needs approval"
	body := fmt.Sprintf("The %s call waited past assistant.permission_timeout with no viewer, so Trebi denied it.", tool)
	hint := "Open the conversation to answer permission requests while you watch."
	acts, _ := json.Marshal([]alerts.InboxAction{
		{Kind: "review", Route: "/v1/conversations/" + conversationID, Label: "Open conversation"},
	})
	_, err := c.Store.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_items (
			id, kind, status, subject, title, body, hint, workspace_id, alert_kind,
			actions, revision, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,1,?,?)`,
			id, string(alerts.InboxApproval), string(alerts.InboxUnread), subject, title, body, hint,
			workspaceID, "assistant_permission_timeout", acts, now, now)
		return err
	}, store.BusEvent{
		Topic: "assistant.permission.timeout", Entity: "conversation", EntityID: conversationID,
		Action: "timeout", Attrs: map[string]string{"conversation": conversationID, "tool": tool},
	})
	return err
}

// onAssistantTurnEnd mails one reply per finished assistant turn when the
// turn began by email. It replaces the run watcher for assistant
// conversations (plan D.9). run_id stays empty: sessions are not runs.
func (c *ControlPlane) onAssistantTurnEnd(ctx context.Context, te assistant.TurnEnd) {
	if c.Channels != nil && !c.Opts.Shadow {
		if err := c.Channels.ReplyOnTurn(ctx, te.ConversationID, ""); err != nil {
			c.log.Warn("assistant channel reply", "conversation", te.ConversationID, "error", err)
		}
	}
	if c.Opts.Shadow || c.Email == nil || !c.Email.Configured() {
		return
	}
	conv, err := c.Conversations.Get(ctx, te.ConversationID)
	if err != nil {
		return
	}
	if conv == nil || te.Channel != conversations.ChannelEmail {
		return
	}
	opts := email.ReplyOpts{ConversationID: te.ConversationID}
	if ws, err := c.Workspaces.Get(ctx, conv.Workspace); err == nil && ws != nil && ws.Assistant != nil {
		opts.FromName = *ws.Assistant
	}
	if err := c.Email.ReplyOnThread(ctx, opts); err != nil {
		c.log.Warn("assistant email reply", "conversation", te.ConversationID, "error", err)
	}
}

// assistantHealthBlock gates assistant queue items on the resolved harness
// health (plan E). "" means ready.
func (c *ControlPlane) assistantHealthBlock(ctx context.Context, workspaceID string) dispatch.BlockReason {
	if c.asstResolve == nil {
		return ""
	}
	eff, err := c.asstResolve.Effective(ctx, workspaceID)
	if err != nil {
		return dispatch.BlockAuthMissing
	}
	if eff.Resolved == "" {
		return dispatch.BlockAuthMissing
	}
	if !eff.Health.Available {
		return dispatch.BlockAuthMissing
	}
	return ""
}

// dispatchAssistant prompts the assistant session for one trigger item and
// marks the item done on the result (plan E). The prompt goes headless, so a
// pending permission times out with an inbox item.
func (c *ControlPlane) dispatchAssistant(ctx context.Context, item *dispatch.QueueItem) {
	if c.Conversations == nil || c.asst == nil {
		_ = c.queue.MarkBlocked(ctx, item.ID, dispatch.BlockJobMissing)
		return
	}
	if reason := c.assistantHealthBlock(ctx, item.WorkspaceID); reason != "" {
		_ = c.queue.MarkBlocked(ctx, item.ID, reason)
		return
	}
	convID, channelType, ref, err := c.assistantConversation(ctx, item)
	if err != nil {
		c.failOrRetry(ctx, item, err, nil)
		return
	}
	text := expandVars(item.Prompt, item.Vars)
	if strings.TrimSpace(text) == "" {
		text = "Event trigger " + item.Connector + " fired."
		if len(item.Vars) > 0 {
			keys := make([]string, 0, len(item.Vars))
			for k := range item.Vars {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			text += "\n"
			for _, k := range keys {
				text += k + ": " + item.Vars[k] + "\n"
			}
		}
	}
	if !c.markAttempt(ctx, item) {
		return
	}
	if _, err := c.asst.Ask(ctx, convID, assistant.PromptInput{
		Text:           text,
		ChannelType:    channelType,
		ChannelRef:     ref,
		IdempotencyKey: item.EventID,
		Headless:       true,
	}, 0); err != nil {
		c.log.Warn("assistant trigger prompt", "queue_id", item.ID, "error", err)
		c.failOrRetry(ctx, item, err, nil)
		return
	}
	c.recordPolicy(ctx, item)
	if err := c.queue.MarkDone(ctx, item.ID, dispatch.ResultLaunched, ""); err != nil {
		c.log.Warn("queue mark done", "queue_id", item.ID, "error", err)
	}
	c.ackChannel(ctx, item, dispatch.ResultLaunched)
}
