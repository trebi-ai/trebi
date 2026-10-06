package runs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/trebi-ai/trebi/internal/tools"
)

// mintActorToken returns 32 random bytes and their SHA-256 hex hash.
func mintActorToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("mint run token: %w", err)
	}
	token = hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// HashActorToken is the deterministic verifier used by the API middleware.
func HashActorToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ActorForToken resolves a live run token. A terminal run's token is dead.
func (s *Service) ActorForToken(ctx context.Context, token string) (tools.Actor, error) {
	if len(token) < 32 {
		return tools.Actor{}, fmt.Errorf("invalid run token")
	}
	row := s.st.DB().QueryRowContext(ctx, `
SELECT id, workspace_id, job_id, `+jobNameSQL+`, kind FROM runs
WHERE actor_token_hash = ? AND status = ?`, HashActorToken(token), string(StatusRunning))
	var a tools.Actor
	var kind string
	if err := row.Scan(&a.RunID, &a.WorkspaceID, &a.JobID, &a.Job, &kind); err != nil {
		return tools.Actor{}, fmt.Errorf("invalid run token")
	}
	a.Name = kind
	a.Kind = kind
	return a, nil
}
