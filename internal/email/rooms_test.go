package email

import (
	"strings"
	"testing"
)

func TestNormalSubject(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Re: Fwd:  Invoice  March": "invoice march",
		"AW: SV: RE[2]: Invoice":   "invoice",
		"invoice":                  "invoice",
		"Re:":                      "",
	} {
		if got := normalSubject(in); got != want {
			t.Errorf("normalSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestThreadForSubjectFallback proves headers win, and a mail with no
// headers joins a thread only when the subject matches and the sender is a
// participant.
func TestThreadForSubjectFallback(t *testing.T) {
	t.Parallel()
	st := openEmailStore(t)
	s := testService(st)
	ctx := t.Context()
	room := MailboxRoom("ws_1", "")
	s.touchThread(ctx, room, "<root@x>", "Invoice", []string{"Ann@Example.com", "ops@example.com"})

	cases := []struct {
		name string
		msg  IngressMessage
		want string
	}{
		{"headers", IngressMessage{MessageID: "<b@x>", InReplyTo: "<other@x>", Subject: "Invoice", From: "ann@example.com"}, "<other@x>"},
		{"subject and participant", IngressMessage{MessageID: "<c@x>", Subject: "Re: invoice", From: "ann@example.com"}, "<root@x>"},
		{"stranger", IngressMessage{MessageID: "<d@x>", Subject: "Invoice", From: "bob@example.com"}, "<d@x>"},
		{"other subject", IngressMessage{MessageID: "<e@x>", Subject: "Lunch", From: "ann@example.com"}, "<e@x>"},
	}
	for _, tc := range cases {
		if got := s.threadFor(ctx, room, tc.msg); got != tc.want {
			t.Errorf("%s: thread %q, want %q", tc.name, got, tc.want)
		}
	}

	// A new CC joins the participants; the title stays.
	s.touchThread(ctx, room, "<root@x>", "Re: Invoice", []string{"carol@example.com", "ann@example.com"})
	var title, parts string
	if err := st.DB().QueryRow(`SELECT title, participants_json FROM channel_threads WHERE instance=? AND thread=?`, Instance, "<root@x>").Scan(&title, &parts); err != nil {
		t.Fatal(err)
	}
	if title != "Invoice" || strings.Join(decodeParticipants(parts), ",") != "ann@example.com,ops@example.com,carol@example.com" {
		t.Fatalf("thread %q %s", title, parts)
	}
}

// TestRoomsListAddresses proves one room per workspace address, one per
// agent with mail, and the query filter.
func TestRoomsListAddresses(t *testing.T) {
	t.Parallel()
	st := openEmailStore(t)
	s := testService(st)
	configureTest(s, "http://127.0.0.1:1")
	ctx := t.Context()
	if _, err := s.EnsureAddress(ctx, "ws_1", "Ellis Labs"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO email_messages (id, workspace_id, agent, direction, disposition, created_at)
		VALUES ('m1','ws_1','research','inbound','routed','2026-09-28T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	rooms, err := s.Rooms(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 2 || rooms[0].ID != MailboxRoom("ws_1", "") || rooms[1].ID != MailboxRoom("ws_1", "research") || rooms[1].Parent != "ws_1" {
		t.Fatalf("rooms %+v", rooms)
	}
	if rooms[0].Kind != RoomKind || !strings.Contains(rooms[0].Name, "@") {
		t.Fatalf("room %+v", rooms[0])
	}
	if got, _ := s.Rooms(ctx, "research"); len(got) != 1 {
		t.Fatalf("filtered rooms %+v", got)
	}
}
