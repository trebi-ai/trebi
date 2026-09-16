package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LedgerEntry is one spawned child, recorded so a crashed daemon can be
// reconciled on boot. StartToken is the positive-match guard against pid
// reuse.
type LedgerEntry struct {
	RunID      string    `json:"run_id"`
	Tool       string    `json:"tool"`
	PID        int       `json:"pid"`
	PGID       int       `json:"pgid,omitempty"`
	StartToken string    `json:"start_token,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	Argv       []string  `json:"argv,omitempty"`
}

// Registry owns live sessions and the pid ledger (Part D).
type Registry struct {
	mu       sync.Mutex
	path     string
	sessions map[string]Session
}

// NewRegistry builds a registry whose ledger lives at path (empty disables
// persistence, for tests).
func NewRegistry(path string) *Registry {
	return &Registry{path: path, sessions: map[string]Session{}}
}

// Add registers a live session and records its pid.
func (r *Registry) Add(runID string, s Session) {
	if r == nil || runID == "" || s == nil {
		return
	}
	r.mu.Lock()
	r.sessions[runID] = s
	r.mu.Unlock()
	argv := []string{}
	if w, ok := s.(*wireSession); ok && w != nil && w.proc != nil && w.proc.cmd != nil {
		argv = append([]string{w.proc.cmd.Path}, w.proc.cmd.Args[1:]...)
	} else if av, ok := s.(interface{ Argv() []string }); ok {
		argv = av.Argv()
	}
	pgid := 0
	if pg, ok := s.(interface{ Pgid() int }); ok {
		pgid = pg.Pgid()
	}
	r.Track(runID, s.Provider(), s.PID(), pgid, argv)
}

// Track records a daemon-owned process in the ledger without a live Session
// (opencode serve helpers and tests). id is namespaced by the caller.
func (r *Registry) Track(id, provider string, pid, pgid int, argv []string) {
	if r == nil || id == "" || pid <= 0 {
		return
	}
	r.writeEntry(LedgerEntry{
		RunID:      id,
		Tool:       provider,
		PID:        pid,
		PGID:       pgid,
		StartedAt:  time.Now().UTC(),
		Argv:       argv,
		StartToken: procStartToken(pid),
	})
}

// Remove unregisters a session and drops its ledger row.
func (r *Registry) Remove(runID string) {
	if r == nil || runID == "" {
		return
	}
	r.mu.Lock()
	delete(r.sessions, runID)
	r.mu.Unlock()
	r.deleteEntry(runID)
}

// Get returns the live session for a run.
func (r *Registry) Get(runID string) (Session, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[runID]
	return s, ok
}

// IDs lists live run ids.
func (r *Registry) IDs() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		out = append(out, id)
	}
	return out
}

// CloseAll gracefully closes every live session (daemon shutdown).
func (r *Registry) CloseAll(ctx context.Context) {
	if r == nil {
		return
	}
	for _, id := range r.IDs() {
		if s, ok := r.Get(id); ok {
			_ = s.Close(ctx)
		}
		r.Remove(id)
	}
}

// List reads the persisted ledger.
func (r *Registry) List() []LedgerEntry {
	if r == nil || r.path == "" {
		return nil
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return nil
	}
	var out []LedgerEntry
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// Reconcile is the boot path: for each ledger row, kill the process only when
// it is positively matched (alive and start token equal); every row is then
// dropped. It returns the run ids whose processes were killed and every run id
// that needs a session_gone row.
func (r *Registry) Reconcile() (killed []string, stale []string) {
	if r == nil || r.path == "" {
		return nil, nil
	}
	entries := r.List()
	for _, e := range entries {
		stale = append(stale, e.RunID)
		if e.PID <= 0 || !procAlive(e.PID) {
			continue
		}
		if e.StartToken != "" {
			if tok := procStartToken(e.PID); tok != "" && tok != e.StartToken {
				// pid reused by an unrelated process: never kill it.
				continue
			}
		}
		killGroup(e.PGID, e.PID)
		killed = append(killed, e.RunID)
	}
	_ = os.Remove(r.path)
	return killed, stale
}

func (r *Registry) writeEntry(e LedgerEntry) {
	if r == nil || r.path == "" || e.PID <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.listLocked()
	replaced := false
	for i := range entries {
		if entries[i].RunID == e.RunID {
			entries[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, e)
	}
	r.writeLocked(entries)
}

func (r *Registry) deleteEntry(runID string) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.listLocked()
	out := entries[:0]
	for _, e := range entries {
		if e.RunID != runID {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		_ = os.Remove(r.path)
		return
	}
	r.writeLocked(out)
}

func (r *Registry) listLocked() []LedgerEntry {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return nil
	}
	var out []LedgerEntry
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func (r *Registry) writeLocked(entries []LedgerEntry) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, r.path)
}
