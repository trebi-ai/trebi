package runs

import (
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/store"
)

// StateOf and activityStateSQL must give the same state for a stored run.
func TestStateOfMatchesSQL(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	statuses := []Status{StatusComplete, StatusFailed, StatusExited, StatusSkipped, StatusTimedOut, StatusParked}
	outcomes := []string{"", "complete", "failed", "skipped"}
	ends := []EndReason{"", EndRestart, EndRequest, EndSentinel}
	for _, s := range statuses {
		for _, o := range outcomes {
			for _, e := range ends {
				var got string
				err := st.DB().QueryRow(`SELECT `+activityStateSQL+` FROM (SELECT ? AS status, ? AS outcome, ? AS end_reason) r`,
					string(s), o, string(e)).Scan(&got)
				if err != nil {
					t.Fatal(err)
				}
				if want := StateOf(s, o, e, ""); ActivityState(got) != want {
					t.Errorf("status %s outcome %q end %s: sql %s, StateOf %s", s, o, e, got, want)
				}
			}
		}
	}
}
