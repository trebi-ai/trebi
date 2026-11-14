package tray

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Notice is one native notification.
type Notice struct {
	ID, Title, Body, Route string
}

// Feed turns inbox bus events into notify and withdraw calls.
type Feed struct {
	base   string
	client *http.Client
	post   func(n Notice)
	pull   func(id string)
	ask    func() // shows the permission prompt
	log    *slog.Logger
	seen   map[string]bool // ids posted by this feed
}

// NewFeed follows the bus of the daemon at base. post shows a notice, pull
// withdraws one, and ask shows the permission prompt.
func NewFeed(base string, post func(Notice), pull func(id string), ask func()) *Feed {
	return &Feed{
		base:   base,
		client: &http.Client{},
		post:   post,
		pull:   pull,
		ask:    ask,
		log:    slog.Default().With("source", "tray_feed"),
		seen:   map[string]bool{},
	}
}

type busEvent struct {
	Topic string `json:"topic"`
	ID    string `json:"id"`
	Attrs struct {
		Status string   `json:"status"` // inbox.created
		Op     string   `json:"op"`     // inbox.updated
		IDs    []string `json:"ids"`
	} `json:"attrs"`
}

const feedBackoff = 2 * time.Second

// Run reads the inbox topics of the bus until ctx ends. It connects again
// after feedBackoff. Each connect starts at the latest event, so a restart
// does not post old items.
func (f *Feed) Run(ctx context.Context) {
	for {
		if err := f.follow(ctx); err != nil && ctx.Err() == nil {
			f.log.Debug("tray.feed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(feedBackoff):
		}
	}
}

func (f *Feed) follow(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.base+"/v1/stream?cursor=latest&topics=inbox.created,inbox.updated,system.notifications_requested", nil)
	if err != nil {
		return err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream: status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev busEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		f.handle(ctx, ev)
	}
	return sc.Err()
}

// handle is one bus event in, at most one notify or some withdraws out. A
// read, archive, or prune withdraws the notices of its ids. A permission
// request calls ask.
func (f *Feed) handle(ctx context.Context, ev busEvent) {
	switch ev.Topic {
	case "system.notifications_requested":
		if f.ask != nil {
			f.ask()
		}
	case "inbox.created":
		if ev.ID == "" || ev.Attrs.Status != "unread" {
			return
		}
		n, err := f.notice(ctx, ev.ID)
		if err != nil {
			f.log.Warn("tray.feed", "id", ev.ID, "err", err)
			return
		}
		f.seen[n.ID] = true
		f.post(n)
	case "inbox.updated":
		switch ev.Attrs.Op {
		case "read", "archive", "prune":
		default:
			return
		}
		for _, id := range ev.Attrs.IDs {
			if f.seen[id] {
				delete(f.seen, id)
				f.pull(id)
			}
		}
	}
}

func (f *Feed) notice(ctx context.Context, id string) (Notice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+"/v1/inbox/"+url.PathEscape(id), nil)
	if err != nil {
		return Notice{}, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return Notice{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Notice{}, fmt.Errorf("inbox item: status %d", resp.StatusCode)
	}
	var item struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		Hint    string `json:"hint"`
		Actions []struct {
			Route string `json:"route"`
		} `json:"actions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return Notice{}, fmt.Errorf("decode inbox item: %w", err)
	}
	n := Notice{ID: item.ID, Title: item.Title, Body: item.Body, Route: "/inbox/" + url.PathEscape(item.ID)}
	if n.Body == "" {
		n.Body = item.Hint
	}
	// An action route that is not an API call is a UI path.
	for _, a := range item.Actions {
		if strings.HasPrefix(a.Route, "/") && !strings.HasPrefix(a.Route, "/v1/") {
			n.Route = a.Route
			break
		}
	}
	return n, nil
}

// Notification permission values that the tray reports to the daemon.
const (
	NotificationsNotDetermined = "not_determined"
	NotificationsDenied        = "denied"
	NotificationsAllowed       = "allowed"
)

// ReportNotifications tells the daemon at base the notification permission.
func ReportNotifications(ctx context.Context, client *http.Client, base, status string) error {
	body, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/v1/system/notifications", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("report notifications: status %d", resp.StatusCode)
	}
	return nil
}
