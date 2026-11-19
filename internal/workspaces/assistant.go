package workspaces

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/store"
	"gopkg.in/yaml.v3"
)

// ErrPrecondition reports a stale If-Match hash on a settings write.
var ErrPrecondition = errors.New("precondition failed")

// AssistantOverride reads the assistant block of trebi.yaml and the file's
// sha256 hash. A missing file or block is an empty override (plan B.3).
func (s *Service) AssistantOverride(_ context.Context, id string) (wire.Override, string, error) {
	ws, err := s.getRaw(context.Background(), id)
	if err != nil {
		return wire.Override{}, "", err
	}
	data, err := os.ReadFile(ws.Path + "/trebi.yaml")
	if err != nil {
		if os.IsNotExist(err) {
			return wire.Override{}, hashBytes(nil), nil
		}
		return wire.Override{}, "", err
	}
	var y trebiYAML
	if err := yaml.Unmarshal(data, &y); err != nil {
		return wire.Override{}, "", fmt.Errorf("trebi.yaml: %w", err)
	}
	ov := wire.Override{}
	if y.Assistant != nil {
		ov = *y.Assistant
	}
	return ov, hashBytes(data), nil
}

// AssistantHasTriggers reports whether the assistant block of a trebi.yaml
// holds a triggers key. Trebi does not run it; the doctor reports it.
func AssistantHasTriggers(trebiYAML []byte) bool {
	var raw struct {
		Assistant map[string]yaml.Node `yaml:"assistant"`
	}
	if yaml.Unmarshal(trebiYAML, &raw) != nil {
		return false
	}
	_, ok := raw.Assistant["triggers"]
	return ok
}

// SetAssistantOverride writes the assistant block of trebi.yaml and returns
// the new hash. A stale ifMatch hash is ErrPrecondition (plan B.3). A write
// emits assistant.settings.updated.
func (s *Service) SetAssistantOverride(ctx context.Context, id string, o wire.Override, ifMatch string) (string, error) {
	ws, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	path := ws.Path + "/trebi.yaml"
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	current := hashBytes(data)
	if ifMatch != "" && ifMatch != current {
		return current, ErrPrecondition
	}
	var y trebiYAML
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &y); err != nil {
			return current, fmt.Errorf("trebi.yaml: %w", err)
		}
	}
	y.ID = ws.ID
	y.Name = ws.Name
	y.GitEnabled = ws.GitEnabled
	if o.Email != nil && o.Email.Style != nil {
		if err := o.Email.Style.Validate(); err != nil {
			return current, err
		}
	}
	y.Assistant = &o
	out, err := yaml.Marshal(&y)
	if err != nil {
		return current, err
	}
	if err := job.WriteAtomic(path, out); err != nil {
		return current, err
	}
	newHash := hashBytes(out)
	s.asstMu.Lock()
	if s.asstWrites == nil {
		s.asstWrites = map[string]string{}
	}
	s.asstWrites[id] = newHash
	s.asstMu.Unlock()
	_, _ = s.st.Append(ctx, store.BusEvent{
		Topic: "assistant.settings.updated", Entity: "workspace", EntityID: id, Action: "updated",
		Attrs: map[string]string{"workspace": id},
	})
	return newHash, nil
}

// WorkspaceInfo returns the identity fields of one workspace.
func (s *Service) WorkspaceInfo(_ context.Context, id string) (wire.WorkspaceInfo, error) {
	ws, err := s.getRaw(context.Background(), id)
	if err != nil {
		return wire.WorkspaceInfo{}, err
	}
	return wire.WorkspaceInfo{ID: ws.ID, Name: ws.Name, Path: ws.Path}, nil
}

// AssistantMemory renders <workspace>/MEMORY.md for the assistant
// instructions (plan D.10). A missing file renders the empty sections.
// The sentinel gate does not apply: a chat has no run to gate.
func (s *Service) AssistantMemory(_ context.Context, workspaceID string) (string, error) {
	ws, err := s.getRaw(context.Background(), workspaceID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(ws.Path, "MEMORY.md"))
	if err != nil {
		data = nil
	}
	def := config.DefaultMemory()
	if c, cerr := config.Load(); cerr == nil {
		def = c.Defaults.Memory
	}
	doc, _ := memory.Parse(data)
	return memory.Render(doc, job.MemoryConfig{}.Resolve(def)), nil
}

// AssistantSettingsChanged reports whether the watcher must still emit
// assistant.settings.updated for a trebi.yaml with this hash. An API write
// emits its own event, so the hash it wrote is skipped.
func (s *Service) AssistantSettingsChanged(_ context.Context, id, hash string) bool {
	s.asstMu.Lock()
	defer s.asstMu.Unlock()
	return s.asstWrites[id] != hash
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// getRaw fetches one row without decoration. Get decorates, and the
// decoration reads the assistant override, so these helpers must not call it.
func (s *Service) getRaw(ctx context.Context, id string) (*Workspace, error) {
	row := s.st.DB().QueryRowContext(ctx, workspaceSelect+` WHERE w.id=?`, id)
	w, err := scanWorkspace(row)
	if err != nil {
		return nil, err
	}
	return &w, nil
}
