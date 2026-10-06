package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
)

const (
	// Instance is the connector instance of the native email channel.
	Instance = "email"
	// subjectWindow is how long a thread stays open for a subject join.
	subjectWindow = 30 * 24 * time.Hour
	// RoomKind is the room kind of a Trebi address.
	RoomKind = "mailbox"
)

// replyPrefixRE matches one reply or forward prefix: Re:, Fwd:, AW:, SV:, RE[2]:.
var replyPrefixRE = regexp.MustCompile(`(?i)^\s*(re|fwd?|aw|sv)(\[\d+\])?\s*:\s*`)

// normalSubject strips reply prefixes, case, and extra space.
func normalSubject(s string) string {
	for {
		next := replyPrefixRE.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = next
	}
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// threadFor finds the thread of an inbound mail. Headers come first. A mail
// with no References and no In-Reply-To joins an open thread of the room
// with the same subject when the sender is a participant. Else it starts a
// new thread.
func (s *Service) threadFor(ctx context.Context, room string, msg IngressMessage) string {
	root := ThreadRoot(msg)
	if len(msg.References) > 0 || msg.InReplyTo != "" {
		return root
	}
	subject := normalSubject(msg.Subject)
	if subject == "" {
		return root
	}
	since := time.Now().Add(-subjectWindow).UTC().Format(time.RFC3339Nano)
	rows, err := s.st.DB().QueryContext(ctx, `SELECT thread, title, participants_json FROM channel_threads
		WHERE instance=? AND room=? AND last_activity_at >= ? ORDER BY last_activity_at DESC`,
		Instance, room, since)
	if err != nil {
		s.log.Warn("email thread lookup", "room", room, "error", err)
		return root
	}
	defer rows.Close()
	sender := strings.ToLower(strings.TrimSpace(msg.From))
	for rows.Next() {
		var thread, title, parts string
		if rows.Scan(&thread, &title, &parts) != nil {
			continue
		}
		if normalSubject(title) == subject && slices.Contains(decodeParticipants(parts), sender) {
			return thread
		}
	}
	return root
}

// touchThread records activity on a thread. The title is set once, by the
// first mail. New participants join in first-seen order.
func (s *Service) touchThread(ctx context.Context, room, thread, title string, people []string) {
	if room == "" || thread == "" {
		return
	}
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		var cur string
		err := tx.QueryRowContext(ctx, `SELECT participants_json FROM channel_threads
			WHERE instance=? AND room=? AND thread=?`, Instance, room, thread).Scan(&cur)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		parts := decodeParticipants(cur)
		for _, p := range people {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" && !slices.Contains(parts, p) {
				parts = append(parts, p)
			}
		}
		b, err := json.Marshal(parts)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err = tx.ExecContext(ctx, `INSERT INTO channel_threads
			(instance, room, thread, title, participants_json, last_activity_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT (instance, room, thread) DO UPDATE SET
				participants_json=excluded.participants_json, last_activity_at=excluded.last_activity_at`,
			Instance, room, thread, title, string(b), now)
		return err
	})
	if err != nil {
		s.log.Warn("email thread upsert", "room", room, "error", err)
	}
}

func decodeParticipants(raw string) []string {
	var out []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out) // a bad row reads as no participants
	}
	return out
}

// Rooms lists the Trebi addresses: one per workspace and one per agent
// subaddress that has mail. The name is the masked address.
func (s *Service) Rooms(ctx context.Context, query string) ([]protocol.Room, error) {
	addrs := map[string]Address{}
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT workspace_id, local_part, domain, created_at, rotated_at, revision FROM email_addresses`)
	if err != nil {
		return nil, fmt.Errorf("email rooms: %w", err)
	}
	var out []protocol.Room
	for rows.Next() {
		a, err := scanAddress(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		addrs[a.WorkspaceID] = a
		m := a
		m.LocalPart = maskLocalPart(a.LocalPart)
		out = append(out, protocol.Room{ID: MailboxRoom(a.WorkspaceID, ""), Name: m.Full(), Kind: RoomKind})
	}
	rows.Close()
	agents, err := s.st.DB().QueryContext(ctx,
		`SELECT DISTINCT workspace_id, `+agentNameSQL+` FROM email_messages em WHERE em.agent_id IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("email rooms: %w", err)
	}
	defer agents.Close()
	for agents.Next() {
		var ws, agent string
		if agents.Scan(&ws, &agent) != nil {
			continue
		}
		a, ok := addrs[ws]
		if !ok {
			continue
		}
		lp, err := tagAddress(a, agent)
		if err != nil {
			continue
		}
		a.LocalPart = maskLocalPart(lp)
		out = append(out, protocol.Room{ID: MailboxRoom(ws, agent), Name: a.Full(), Kind: RoomKind, Parent: ws})
	}
	if query = strings.ToLower(strings.TrimSpace(query)); query != "" {
		out = slices.DeleteFunc(out, func(r protocol.Room) bool {
			return !strings.Contains(strings.ToLower(r.Name), query) && !strings.Contains(strings.ToLower(r.ID), query)
		})
	}
	return out, agents.Err()
}
