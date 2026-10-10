package artifacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
)

// Deliver is where a scheduled refresh sends its snapshot.
type Deliver struct {
	Email *DeliverEmail `json:"email,omitempty"`
}

// DeliverEmail sends the snapshot through the native email channel.
type DeliverEmail struct {
	To      []string `json:"to"`
	Subject string   `json:"subject,omitempty"`
}

// Scheduler writes the schedule row of a live artifact (D18).
type Scheduler interface {
	SetArtifact(ctx context.Context, workspaceID, artifactID, spec, tz string) error
	DeleteArtifact(ctx context.Context, artifactID string) error
}

// WithSchedule wires SetSchedule.
func (s *Service) WithSchedule(sc Scheduler) *Service {
	s.sched = sc
	return s
}

// ScheduleOpts is the schedule of a live artifact. An empty Spec removes it.
type ScheduleOpts struct {
	Spec     string // "@every 24h" or a 5-field cron
	Timezone string
	Deliver  *Deliver
}

// SetSchedule sets or removes the scheduled refresh of a live artifact.
func (s *Service) SetSchedule(ctx context.Context, id string, o ScheduleOpts) (Artifact, error) {
	if s.sched == nil {
		return Artifact{}, fmt.Errorf("artifact schedules are not wired")
	}
	a, err := s.Get(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	if o.Spec != "" && !a.Live {
		return Artifact{}, invalid("schedule", "%s is not a live artifact", a.Name)
	}
	if err := o.Deliver.check(); err != nil {
		return Artifact{}, err
	}
	deliver := ""
	if o.Spec != "" && o.Deliver != nil && o.Deliver.Email != nil {
		b, _ := json.Marshal(o.Deliver)
		deliver = string(b)
	}
	if o.Spec == "" {
		err = s.sched.DeleteArtifact(ctx, id)
	} else {
		err = s.sched.SetArtifact(ctx, a.Workspace, id, o.Spec, o.Timezone)
	}
	if err != nil {
		return Artifact{}, invalid("schedule", "%v", err)
	}
	a.Revision++
	_, err = s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE artifacts SET schedule=?, deliver_json=?, revision=?, updated_at=? WHERE id=?`,
			o.Spec, deliver, a.Revision, fmtTime(s.now()), id)
		return err
	}, s.event("artifact.updated", "updated", &a, a.LatestVersion))
	if err != nil {
		return Artifact{}, err
	}
	return s.Get(ctx, id)
}

func (d *Deliver) check() error {
	if d == nil || d.Email == nil {
		return nil
	}
	if len(d.Email.To) == 0 {
		return invalid("deliver.email.to", "needs at least one address")
	}
	for _, to := range d.Email.To {
		if _, err := mail.ParseAddress(to); err != nil {
			return invalid("deliver.email.to", "%q is not an address", to)
		}
	}
	return nil
}

// PageURL is the UI page of version n, or "" when the daemon has no
// address. snapshot is optional.
func PageURL(id string, n int, snapshot string) string {
	base := home.APIJSONURL()
	if cfg, err := config.Load(); err == nil && cfg != nil && cfg.UI.PublicURL != "" {
		base = cfg.UI.PublicURL
	}
	if base == "" {
		return ""
	}
	u := fmt.Sprintf("%s/artifacts/%s?v=%d", strings.TrimRight(base, "/"), id, n)
	if snapshot != "" {
		u += "&snapshot=" + snapshot
	}
	return u
}
