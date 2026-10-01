package connectors

import (
	"context"
	"fmt"
	"strings"
)

// Sender is one identity that sent events to a connection.
type Sender struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
	LastAt   string `json:"last_at"`
	Count    int    `json:"count"`
}

// Senders lists the identities in the events of one instance, the most
// recent first. q filters on the identity or the name. limit defaults to 50
// and is at most 200.
func (s *Service) Senders(ctx context.Context, instance, q string, limit int) ([]Sender, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	query := `SELECT identity, COALESCE(MAX(sender),''), MAX(received_at), COUNT(*) FROM events
		WHERE instance=? AND identity IS NOT NULL AND identity<>''`
	args := []any{instance}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + q + "%"
		query += ` AND (identity LIKE ? OR sender LIKE ?)`
		args = append(args, like, like)
	}
	query += ` GROUP BY identity ORDER BY MAX(received_at) DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.st.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("senders of %s: %w", instance, err)
	}
	defer rows.Close()
	out := []Sender{}
	for rows.Next() {
		var snd Sender
		if err := rows.Scan(&snd.Identity, &snd.Name, &snd.LastAt, &snd.Count); err != nil {
			return nil, err
		}
		out = append(out, snd)
	}
	return out, rows.Err()
}
