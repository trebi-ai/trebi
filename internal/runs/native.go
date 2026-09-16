package runs

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/runtime/native"
	"github.com/trebi-ai/trebi/internal/tools"
)

const nativeHarness = "native"

func (s *Service) launchNative(ctx context.Context, opts LaunchOpts) (*Run, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Assistant.Model) != "" && !cfg.Assistant.Enabled() {
		opts.Harness = nativeHarness
		opts.Runtime = nativeHarness
		run, err := s.Create(ctx, opts.CreateOpts)
		if err != nil {
			return nil, err
		}
		_ = s.Terminate(ctx, run.ID, StatusFailed, EndAuth, nil)
		return s.Get(ctx, run.ID)
	}
	prov := s.prov
	if prov == nil {
		prov, err = llm.New(cfg.Assistant)
		if err != nil {
			opts.Harness = nativeHarness
			opts.Runtime = nativeHarness
			run, cerr := s.Create(ctx, opts.CreateOpts)
			if cerr != nil {
				return nil, cerr
			}
			end := EndLaunch
			if errors.Is(err, llm.ErrUnauthorized) || errors.Is(err, config.ErrNoAssistantKey) {
				end = EndAuth
			}
			_ = s.Terminate(ctx, run.ID, StatusFailed, end, nil)
			return s.Get(ctx, run.ID)
		}
	}
	opts.Harness = nativeHarness
	opts.Runtime = nativeHarness
	opts.TmuxSession = ""
	run, err := s.Create(ctx, opts.CreateOpts)
	if err != nil {
		return nil, err
	}
	s.AddEvent(ctx, run.ID, "launch", "runner", map[string]string{
		"harness": nativeHarness, "mode": nativeHarness, "reason": "config: assistant.model",
		"model": cfg.Assistant.Model,
	})
	s.log.Info("native assistant launch", "id", run.ID, "model", cfg.Assistant.Model, "runtime_reason", "config: assistant.model")

	life := s.life
	if life == nil {
		life = context.Background()
	}
	loopCtx, cancel := context.WithCancel(life)
	inbox := make(chan string, 8)
	s.registerNative(run.ID, cancel, inbox)

	tr, _ := native.OpenTranscript(run.ID)
	go s.runNative(loopCtx, run, opts, cfg, prov, inbox, tr)
	return s.Get(ctx, run.ID)
}

func (s *Service) runNative(ctx context.Context, run *Run, opts LaunchOpts, cfg *config.Config, prov llm.Provider, inbox <-chan string, tr *native.Transcript) {
	defer s.unregisterNative(run.ID)
	defer tr.Close()
	deny := []string{}
	if cfg.Assistant.APIKeyEnv != "" {
		deny = append(deny, cfg.Assistant.APIKeyEnv)
	}
	var reg *tools.Registry
	if s.dispatch != nil {
		reg = tools.NewNativeRegistry(s.dispatch)
	}
	err := native.Run(ctx, native.RunOpts{
		Provider:      prov,
		RunID:         run.ID,
		ConvID:        run.ConversationID,
		WorkspaceRoot: opts.WorkingDir,
		WorkingDir:    opts.WorkingDir,
		System:        sysOrDefault(cfg.Assistant.SystemExtra),
		Prompt:        opts.Prompt,
		Model:         cfg.Assistant.Model,
		MaxTokens:     cfg.Assistant.MaxTokensOrDefault(),
		Headers:       cfg.Assistant.Headers,
		MaxTurns:      cfg.Assistant.Limits.MaxTurns,
		ContextTokens: cfg.Assistant.ContextTokens,
		DailyLimit:    cfg.Assistant.Limits.DailyTokens,
		Inbox:         inbox,
		MaxFileBytes:  cfg.Files.MaxBytesOrDefault(),
		Registry:      reg,
		Env:           nativeEnv(deny),
		CheckDaily:    s.checkDaily(cfg.Assistant.Limits.DailyTokens),
		Transcript:    tr,
		Log:           s.log,
		Sink: native.Sink{
			AddEvent: func(ctx context.Context, kind, source string, detail any) {
				s.AddEvent(ctx, run.ID, kind, source, detail)
			},
			Ingest: func(ctx context.Context, texts []string) error {
				if s.ingest == nil {
					return nil
				}
				return s.ingest(ctx, run.ID, run.ConversationID, texts)
			},
			Recap: func(ctx context.Context, text string) error {
				if s.recap == nil {
					return nil
				}
				return s.recap(ctx, run.ConversationID, text)
			},
			SetAgent: func(ctx context.Context, state string) {
				s.SetSessionState(ctx, run.ID, SessionState(state), "native")
			},
			Finish: func(ctx context.Context, status, end string, u llm.Usage) error {
				tok := &pricing.TokenUsage{
					Input: u.Input, Output: u.Output,
					CacheRead: u.CacheRead, CacheCreation: u.CacheCreation,
					Model: cfg.Assistant.Model,
				}
				return s.Terminate(ctx, run.ID, Status(status), EndReason(end), tok)
			},
			Gate: nativeMemoryGate(s, run, opts),
		},
	})
	if err != nil && ctx.Err() == nil {
		s.log.Warn("native loop", "run", run.ID, "error", err)
	}
}

func sysOrDefault(extra string) string {
	base := "You are the Trebi workspace assistant. Use the registered trebi tools for control-plane operations and workspace tools for files.\n\n" + tools.Instructions
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return base
	}
	return base + "\n\n" + extra
}

func (s *Service) registerNative(id string, cancel context.CancelFunc, inbox chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.native == nil {
		s.native = map[string]*nativeHandle{}
	}
	s.native[id] = &nativeHandle{cancel: cancel, inbox: inbox}
}

func (s *Service) unregisterNative(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.native, id)
}

func (s *Service) nativeHandle(id string) *nativeHandle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.native[id]
}

func (s *Service) cancelNative(id string) {
	if h := s.nativeHandle(id); h != nil && h.cancel != nil {
		h.cancel()
	}
}

func (s *Service) injectNative(id, text string) bool {
	h := s.nativeHandle(id)
	if h == nil {
		return false
	}
	select {
	case h.inbox <- text:
		return true
	default:
		return false
	}
}

func (s *Service) wantsNative(opts LaunchOpts) bool {
	if opts.Kind != KindAssistant {
		return false
	}
	if strings.EqualFold(opts.Runtime, "tui") {
		return false
	}
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg.Assistant.Model) != ""
}

func (s *Service) checkDaily(limit int) func(context.Context, int64) (bool, error) {
	if limit <= 0 {
		return nil
	}
	return func(ctx context.Context, add int64) (bool, error) {
		total, err := s.addAssistantUsage(ctx, add)
		if err != nil {
			return false, err
		}
		return total <= int64(limit), nil
	}
}

func (s *Service) addAssistantUsage(ctx context.Context, add int64) (int64, error) {
	day := time.Now().UTC().Format("2006-01-02")
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO assistant_usage(day, tokens) VALUES(?,?)
		ON CONFLICT(day) DO UPDATE SET tokens = tokens + excluded.tokens`, day, add)
	if err != nil {
		return 0, err
	}
	var total int64
	err = s.st.DB().QueryRowContext(ctx, `SELECT tokens FROM assistant_usage WHERE day=?`, day).Scan(&total)
	return total, err
}

// RecoverNative terminates running native rows that have no loop (daemon restart).
func (s *Service) RecoverNative(ctx context.Context) {
	list, err := s.List(ctx, ListOpts{Status: string(StatusRunning), Limit: 500})
	if err != nil {
		return
	}
	for _, run := range list {
		if run.Harness != nativeHarness {
			continue
		}
		if s.nativeHandle(run.ID) != nil {
			continue
		}
		_ = s.Terminate(ctx, run.ID, StatusExited, EndSessionGone, nil)
		s.log.Info("native run recovered", "id", run.ID, "end_reason", EndSessionGone)
	}
}

// nativeMemoryGate builds the Sink.Gate closure for one native run: validate
// at the completion point, inject a fix message within the budget, then let
// the run finish with a memory.invalid record (B22 §3.3).
func nativeMemoryGate(s *Service, run *Run, opts LaunchOpts) func(ctx context.Context) string {
	fixAttempts := 0
	return func(ctx context.Context) string {
		if opts.Memory == nil {
			return ""
		}
		issues := s.gateMemory(ctx, run, opts.Memory)
		if len(issues) == 0 {
			return ""
		}
		if fixAttempts >= maxMemoryFixes {
			s.recordMemoryInvalid(ctx, run, issues, "native")
			return ""
		}
		fixAttempts++
		s.requestMemoryFix(ctx, run.ID, "native", fixAttempts, issues)
		return fixMessage(issues, run.ID)
	}
}

func nativeEnv(deny []string) []string {
	skip := map[string]bool{"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true}
	for _, k := range deny {
		skip[k] = true
	}
	var out []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if skip[k] {
			continue
		}
		out = append(out, e)
	}
	return out
}
