package email

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
)

type fakeArtifacts map[string]ArtifactBody

func (f fakeArtifacts) Body(_ context.Context, ref ArtifactRef) (ArtifactBody, error) {
	return f[ref.ID], nil
}

func TestSendArtifact(t *testing.T) {
	t.Parallel()
	fw := &fakeWorker{token: "test-token-0000"}
	srv := httptest.NewServer(fw.handler())
	t.Cleanup(srv.Close)
	st := openEmailStore(t)
	s := testService(st).WithArtifacts(fakeArtifacts{
		"art_md":   {Kind: "markdown", Name: "weekly", Title: "Weekly report", Content: []byte("Sales are **up**.")},
		"art_mail": {Kind: "email", Name: "hello", Title: "hello", Content: []byte("Hello **team**.")},
		"art_html": {Kind: "html", Name: "big", Title: "Big", Content: []byte("<p>" + strings.Repeat("x", ClipBytes) + "</p>")},
		"art_csv":  {Kind: "table", Name: "sales", Title: "Sales", FileName: "sales.csv", MIME: "text/csv", Content: []byte("a,b\n")},
	})
	configureTest(s, srv.URL)
	if _, err := s.EnsureAddress(t.Context(), "ws_1", "ellis-labs"); err != nil {
		t.Fatal(err)
	}
	send := func(id, subject string) SendRequest {
		t.Helper()
		q, err := s.Send(t.Context(), SendOpts{WorkspaceID: "ws_1", To: []string{"ops@example.com"}, Subject: subject,
			Artifact: &ArtifactRef{ID: id, Version: 2, URL: "https://trebi.example/artifacts/" + id + "?v=2"}})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		flush(t, s)
		var sent SendRequest
		if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
			t.Fatal(err)
		}
		var aid string
		var n int
		if err := st.DB().QueryRow(`SELECT artifact_id, artifact_version FROM email_messages WHERE message_id=?`, q.MessageID).Scan(&aid, &n); err != nil || aid != id || n != 2 {
			t.Fatalf("%s row: %s %d %v", id, aid, n, err)
		}
		return sent
	}

	if sent := send("art_md", ""); sent.Subject != "Weekly report" || !strings.Contains(sent.HTML, ArtifactLinkLabel) ||
		!strings.Contains(sent.HTML, "art_md?v=2") || !strings.Contains(sent.HTML, "<strong") {
		t.Errorf("markdown: %q %q", sent.Subject, sent.HTML)
	}
	if sent := send("art_mail", "Hi"); sent.Subject != "Hi" || strings.Contains(sent.HTML, ArtifactLinkLabel) || sent.Text != "Hello **team**." {
		t.Errorf("draft: %q %q", sent.Subject, sent.HTML)
	}
	if sent := send("art_html", ""); len(sent.Attachments) != 1 || sent.Attachments[0].Name != "big-v2.html" || len(sent.HTML) > ClipBytes {
		t.Errorf("large html: %d bytes, %+v", len(sent.HTML), sent.Attachments)
	}
	if sent := send("art_csv", ""); len(sent.Attachments) != 1 || sent.Attachments[0].Name != "sales.csv" || !strings.Contains(sent.Text, "Sales is attached.") {
		t.Errorf("table: %q %+v", sent.Text, sent.Attachments)
	}
}

func TestBuildMIME(t *testing.T) {
	t.Parallel()
	s := New(openEmailStore(t))
	raw, id, err := s.BuildMIME(Draft{From: "me@example.com", To: []string{"a@example.com"}, Cc: []string{"b@example.com"},
		Subject: "Grüße", InReplyTo: "<x@example.com>", Text: "Hello **team**."})
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if m.Header.Get("Message-ID") != id || !strings.HasSuffix(id, "@example.com>") || subject != "Grüße" ||
		m.Header.Get("Cc") != "b@example.com" || m.Header.Get("In-Reply-To") != "<x@example.com>" {
		t.Fatalf("headers = %v", m.Header)
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	var types []string
	var html string
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p) // the reader decodes quoted-printable
		types = append(types, strings.Split(p.Header.Get("Content-Type"), ";")[0])
		if strings.HasPrefix(p.Header.Get("Content-Type"), "text/html") {
			html = string(b)
		}
	}
	if len(types) != 2 || types[0] != "text/plain" || types[1] != "text/html" || !strings.Contains(html, "<strong") {
		t.Fatalf("parts = %v, html %q", types, html)
	}
}
