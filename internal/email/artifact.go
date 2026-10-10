package email

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/store"
)

// ArtifactRef names the artifact version that a mail carries (D8).
type ArtifactRef struct {
	ID         string
	Version    int
	SnapshotID string
	URL        string // the UI page of the version; "" adds no link
}

// ArtifactBody is the content of one artifact version.
type ArtifactBody struct {
	Kind     string // markdown, email, html, or another artifact kind
	Name     string
	Title    string
	FileName string
	MIME     string
	Content  []byte
}

// ArtifactReader reads the version that a mail carries.
type ArtifactReader interface {
	Body(ctx context.Context, ref ArtifactRef) (ArtifactBody, error)
}

// WithArtifacts wires SendOpts.Artifact.
func (s *Service) WithArtifacts(r ArtifactReader) *Service {
	s.arts = r
	return s
}

// ArtifactLinkLabel is the button text of an artifact mail.
const ArtifactLinkLabel = "Open the full report"

// artifactParts fills the text, the HTML, and the files of an artifact mail.
// A mail that does not fit gets the version as <name>-v<n>.html.
func (s *Service) artifactParts(ctx context.Context, cfg Settings, o *SendOpts) error {
	if s.arts == nil {
		return errors.New("artifact mail is not wired")
	}
	if strings.TrimSpace(o.HTML) != "" {
		return fmt.Errorf("%w: an artifact mail takes text, not html", ErrContent)
	}
	b, err := s.arts.Body(ctx, *o.Artifact)
	if err != nil {
		return err
	}
	o.Subject = cmp.Or(strings.TrimSpace(o.Subject), b.Title, b.Name)
	lead := strings.TrimSpace(o.Text)
	page := Page{Layout: LayoutReport, Title: o.Subject, Brand: cmp.Or(o.FromName, cfg.FromName),
		Link: o.Artifact.URL, LinkLabel: ArtifactLinkLabel}
	copyName := fmt.Sprintf("%s-v%d.html", b.Name, o.Artifact.Version)
	var md string
	switch b.Kind {
	case "email":
		md, page.Layout, page.Link = string(b.Content), LayoutReply, ""
	case "markdown":
		md = strings.TrimSpace(lead + "\n\n" + string(b.Content))
	case "html":
		if len(b.Content) <= ClipBytes {
			o.HTML = string(b.Content)
			o.Text = cmp.Or(lead, PlainText(o.HTML), o.Subject)
			return nil
		}
		md = cmp.Or(lead, fmt.Sprintf("%s is too large for one mail. The full report is attached.", b.Title))
		o.Attachments = append(o.Attachments, SendAttachment{Name: copyName, Type: "text/html", Content: b.Content})
	default:
		md = cmp.Or(lead, fmt.Sprintf("%s is attached.", b.Title))
		o.Attachments = append(o.Attachments, SendAttachment{Name: b.FileName, Type: b.MIME, Content: b.Content})
	}
	r, err := s.htmlPart(ctx, o.WorkspaceID, o.Agent, md, page)
	if err != nil {
		return err
	}
	o.Text = md
	if r.Full == "" {
		o.HTML = r.HTML
		return nil
	}
	if len(o.Attachments) >= MaxAttachments {
		o.HTML = r.Full // no room for the copy; send the whole mail
		return nil
	}
	o.HTML = r.HTML
	o.Attachments = append(o.Attachments, SendAttachment{Name: copyName, Type: "text/html", Content: []byte(r.Full)})
	return nil
}

// Draft is a mail that a connector sends or saves (C3).
type Draft struct {
	From      string
	To        []string
	Cc        []string
	Subject   string
	InReplyTo string
	Text      string // Markdown
}

// BuildMIME renders d as one RFC 5322 message with a text and an HTML part.
// The From header is d.From; a mailbox connector replaces it with its own
// account when it is not an address.
func (s *Service) BuildMIME(d Draft) ([]byte, string, error) {
	r, err := s.render.Markdown(d.Text, Page{Layout: LayoutReply, Title: d.Subject})
	if err != nil {
		return nil, "", err
	}
	html := cmp.Or(r.Full, r.HTML)
	domain := "trebi.local"
	if a, err := mail.ParseAddress(d.From); err == nil {
		if _, host, ok := strings.Cut(a.Address, "@"); ok {
			domain = host
		}
	}
	msgID := MintMessageID(domain)
	boundary := "trebi-" + strings.ToLower(ulid.Make().String())
	var b bytes.Buffer
	hdr := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	hdr("From", d.From)
	hdr("To", strings.Join(d.To, ", "))
	hdr("Cc", strings.Join(d.Cc, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", d.Subject))
	hdr("Date", time.Now().UTC().Format(time.RFC1123Z))
	hdr("Message-ID", msgID)
	hdr("In-Reply-To", d.InReplyTo)
	hdr("References", d.InReplyTo)
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, p := range [][2]string{{"text/plain", d.Text}, {"text/html", html}} {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary, p[0])
		w := quotedprintable.NewWriter(&b)
		if _, err := w.Write([]byte(p[1])); err != nil {
			return nil, "", err
		}
		if err := w.Close(); err != nil {
			return nil, "", err
		}
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), msgID, nil
}

// SentByConnector records a mail that a mailbox connection sent, so that
// the Sent list and the artifact deliveries show it.
type SentByConnector struct {
	WorkspaceID string
	MessageID   string
	InReplyTo   string
	To          []string
	Subject     string
	RunID       string
	Artifact    *ArtifactRef
}

// RecordSent writes the email_messages row of a connector send.
func (s *Service) RecordSent(ctx context.Context, m SentByConnector) (string, error) {
	id := ulid.Make().String()
	var art ArtifactRef
	var version any
	if m.Artifact != nil {
		art, version = *m.Artifact, m.Artifact.Version
	}
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO email_messages
  (id, direction, workspace_id, message_id, in_reply_to, thread_root, recipients, subject, disposition, run_id,
   purpose, created_at, artifact_id, artifact_version, snapshot_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "out", m.WorkspaceID, m.MessageID, m.InReplyTo, m.MessageID, strings.Join(m.To, ","), m.Subject,
		dispositionSent, store.NullStr(m.RunID), "artifact", time.Now().UTC().Format(time.RFC3339),
		art.ID, version, art.SnapshotID)
	if err != nil {
		return "", fmt.Errorf("record connector mail: %w", err)
	}
	return id, nil
}
