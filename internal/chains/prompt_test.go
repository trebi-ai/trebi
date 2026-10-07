package chains

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

// TestPromptGolden pins the prompt texts of Part I.
func TestPromptGolden(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("Revenue grows. ", maxReply/15+10)
	cases := map[string]string{
		"ask":           askMessage(KindAsk, "hop_01JAC", "ceo", "01JAB", 1, "What is the Q3 budget?", "Board meets Friday."),
		"ask_human":     askMessage(KindAsk, "hop_01JAC", "", "", 0, "What is the Q3 budget?", ""),
		"handoff":       askMessage(KindHandoff, "hop_01JAG", "ceo", "01JAB", 1, "Review docs/nda.md.", "The vendor signs Monday."),
		"reply":         replyPrompt("cfo", "hop_01JAF", "01JAD", 1, "The budget is $40k."),
		"reply_cut":     replyPrompt("cfo", "hop_01JAF", "01JAD", 1, long)[maxReply-40:],
		"asked":         askedMessage(KindAsk, "cfo", "hop_01JAC"),
		"handed_off":    askedMessage(KindHandoff, "legal", "hop_01JAG"),
		"held":          heldMessage("legal", "hop_01JAH"),
		"no_reply":      noReplyText("cfo"),
		"failed_reply":  failedReplyText("cfo", "timeout"),
		"stopped_reply": stoppedReplyText,
		"limit":         limitText,
		"title_hops":    limitTitle(RefuseHops, Limits{MaxHops: 5}),
		"title_usd":     limitTitle(RefuseUSD, Limits{MaxUSD: 2.5}),
		"title_turns":   limitTitle(RefuseTurns, Limits{MaxTurns: 20}),
	}
	for name, got := range cases {
		golden := filepath.Join("testdata", "prompts", name+".golden")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got, want)
		}
	}
}
