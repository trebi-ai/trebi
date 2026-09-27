package assistant

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"
	"github.com/trebi-ai/agent-wire/native/provider/anthropic"
	"github.com/trebi-ai/agent-wire/native/provider/openai"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/tools"
)

// NewNativeModel builds the provider model for the API format (plan
// 2026-09-26 B). The driver and the settings probe share it.
func NewNativeModel(c agentwire.Credentials, model string) (native.Model, error) {
	switch c.Format {
	case "anthropic":
		return anthropic.New(c, model)
	default:
		return openai.New(c, model)
	}
}

// RegisterNativeDriver builds the agent-wire native driver and registers it
// on the runtime (plan 2026-09-26 I). The provider follows the API format
// in the live config, so a save lands without a restart. The trebi tool
// catalog opens as one in-process MCP session per conversation.
func RegisterNativeDriver(rt *agentwire.Runtime, cfgFn func() config.NativeConfig, disp tools.Dispatcher, st *Store, maxFileBytes int64, log *slog.Logger) *native.Driver {
	if log == nil {
		log = slog.Default()
	}
	if cfgFn == nil {
		cfgFn = func() config.NativeConfig { return config.NativeConfig{} }
	}
	cfg := cfgFn()
	daily := cfg.Limits.DailyTokens
	drvCfg := native.Config{
		NewModel: NewNativeModel,
		Credentials: func() (agentwire.Credentials, error) {
			n := cfgFn()
			key, _, err := n.ResolveKey()
			if err != nil {
				return agentwire.Credentials{}, err
			}
			return agentwire.Credentials{
				Format:  n.Format(),
				BaseURL: n.BaseURL,
				APIKey:  key,
				Headers: n.Headers,
			}, nil
		},
		ToolSetFuncs:    []func(context.Context) (native.ToolSet, error){nativeToolSet(disp)},
		BuiltinTools:    true,
		MaxFileBytes:    maxFileBytes,
		Store:           native.FileStore(filepath.Join(home.Base(), "assistant", "native")),
		Compactor:       native.SummaryCompactor(20),
		ContextTokens:   cfg.ContextTokens,
		MaxOutputTokens: cfg.MaxTokens,
		// The daily cap reads the DB session rows, so it survives a
		// restart. Bookkeeping never blocks a call.
		BeforeCall: func(ctx context.Context, _ string) error {
			return NativeDailyCapGuard(ctx, st, daily)
		},
	}
	drv := native.NewDriver(drvCfg)
	rt.SetDriver(agentwire.Native, drv)
	return drv
}

// NativeDailyCapGuard is the driver's BeforeCall: it fails a model call
// once today's native tokens reach the daily cap. A cap of zero disables
// the guard, and bookkeeping errors never block a call.
func NativeDailyCapGuard(ctx context.Context, st *Store, daily int) error {
	if st == nil || daily <= 0 {
		return nil
	}
	today, err := st.TokensToday(ctx, string(agentwire.Native))
	if err != nil || today < int64(daily) {
		return nil
	}
	return fmt.Errorf("daily token cap of %d reached for the native assistant", daily)
}

// nativeToolSet opens one in-process MCP tool catalog per native session
// (plan 2026-09-26 I.7). The dispatcher is late-bound, so the registry is
// built per call, and the native deny-list applies.
func nativeToolSet(disp tools.Dispatcher) func(context.Context) (native.ToolSet, error) {
	return func(ctx context.Context) (native.ToolSet, error) {
		if disp == nil {
			// No dispatcher (tests, embedded runs): the loop runs with the
			// built-in file tools only.
			return native.StaticTools("trebi"), nil
		}
		reg := tools.NewNativeRegistry(disp)
		srv := reg.MCPServer(&mcp.Implementation{Name: "trebi", Version: "1"})
		srvT, cliT := mcp.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, srvT, nil); err != nil {
			return nil, fmt.Errorf("native: connect tool server: %w", err)
		}
		cli := mcp.NewClient(&mcp.Implementation{Name: "trebi-native", Version: "1"}, nil)
		cs, err := cli.Connect(ctx, cliT, nil)
		if err != nil {
			return nil, fmt.Errorf("native: connect tool client: %w", err)
		}
		return native.MCPSessionToolSet("trebi", cs), nil
	}
}
