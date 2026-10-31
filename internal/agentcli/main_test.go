//go:build !live_tui

package agentcli

import (
	"testing"

	"github.com/trebi-ai/trebi/internal/hometest"
)

func TestMain(m *testing.M) { hometest.Main(m) }
