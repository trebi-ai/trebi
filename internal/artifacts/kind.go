package artifacts

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind is the content type of an artifact.
type Kind string

const (
	KindMarkdown Kind = "markdown"
	KindEmail    Kind = "email"
	KindHTML     Kind = "html"
	KindTable    Kind = "table"
	KindJSON     Kind = "json"
	KindCode     Kind = "code"
	KindSVG      Kind = "svg"
	KindImage    Kind = "image"
	KindPDF      Kind = "pdf"
	KindFile     Kind = "file"
)

// MaxGridRows is the largest table that the UI shows as a grid.
const MaxGridRows = 10_000

func (k Kind) Valid() bool {
	switch k {
	case KindMarkdown, KindEmail, KindHTML, KindTable, KindJSON, KindCode, KindSVG, KindImage, KindPDF, KindFile:
		return true
	}
	return false
}

// Editable reports whether the UI can edit the source of k.
func (k Kind) Editable() bool {
	switch k {
	case KindMarkdown, KindEmail, KindHTML, KindJSON, KindCode, KindTable, KindSVG:
		return true
	}
	return false
}

// FieldError is one validation failure. Field names the header field or
// "content".
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// InvalidError lists the validation failures of one save.
type InvalidError struct{ Fields []FieldError }

func (e *InvalidError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "invalid artifact: " + strings.Join(parts, "; ")
}

type kindMIME struct {
	kind Kind
	mime string
}

//nolint:gochecknoglobals // static extension table, never mutated
var extKinds = map[string]kindMIME{
	".md": {KindMarkdown, mdMIME}, ".markdown": {KindMarkdown, mdMIME}, ".mdx": {KindMarkdown, mdMIME},
	".html": {KindHTML, "text/html; charset=utf-8"}, ".htm": {KindHTML, "text/html; charset=utf-8"},
	".csv": {KindTable, "text/csv; charset=utf-8"}, ".tsv": {KindTable, "text/tab-separated-values; charset=utf-8"},
	".json": {KindJSON, "application/json"}, ".svg": {KindSVG, "image/svg+xml"}, ".pdf": {KindPDF, "application/pdf"},
	".png": {KindImage, "image/png"}, ".jpg": {KindImage, "image/jpeg"}, ".jpeg": {KindImage, "image/jpeg"},
	".gif": {KindImage, "image/gif"}, ".webp": {KindImage, "image/webp"},
}

//nolint:gochecknoglobals // static extension table, never mutated
var codeExt = map[string]bool{
	".yml": true, ".yaml": true, ".toml": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true,
	".jsx": true, ".tsx": true, ".py": true, ".sh": true, ".bash": true, ".txt": true, ".sql": true,
	".css": true, ".go": true, ".jsonc": true,
}

const mdMIME = "text/markdown; charset=utf-8"

// DetectKind finds the kind and MIME type from the file name and the first
// bytes of the content.
func DetectKind(fileName string, head []byte) (Kind, string) {
	ext := strings.ToLower(filepath.Ext(fileName))
	if km, ok := extKinds[ext]; ok {
		if km.kind == KindMarkdown && isEmailDraft(head) {
			return KindEmail, mdMIME
		}
		return km.kind, km.mime
	}
	if codeExt[ext] {
		return KindCode, "text/plain; charset=utf-8"
	}
	sniff := http.DetectContentType(head)
	if strings.HasPrefix(sniff, "image/") {
		return KindImage, sniff
	}
	// Text with no extension is Markdown, the default of a report.
	if ext == "" && strings.HasPrefix(sniff, "text/plain") {
		if isEmailDraft(head) {
			return KindEmail, mdMIME
		}
		return KindMarkdown, mdMIME
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return KindFile, t
	}
	return KindFile, "application/octet-stream"
}

// MIME is the content type of k for a file name with no known extension.
func (k Kind) MIME() string {
	switch k {
	case KindMarkdown, KindEmail:
		return "text/markdown; charset=utf-8"
	case KindHTML:
		return "text/html; charset=utf-8"
	case KindTable:
		return "text/csv; charset=utf-8"
	case KindJSON:
		return "application/json"
	case KindCode:
		return "text/plain; charset=utf-8"
	case KindSVG:
		return "image/svg+xml"
	case KindPDF:
		return "application/pdf"
	}
	return "application/octet-stream"
}

// Ext is the file extension of k, for a file name that the user did not give.
func (k Kind) Ext() string {
	switch k {
	case KindMarkdown, KindEmail:
		return ".md"
	case KindHTML:
		return ".html"
	case KindTable:
		return ".csv"
	case KindJSON:
		return ".json"
	case KindCode:
		return ".txt"
	case KindSVG:
		return ".svg"
	case KindPDF:
		return ".pdf"
	}
	return ""
}

// Validate checks content against the rules of k. A table over MaxGridRows
// is valid; the UI only offers it as a download.
func (k Kind) Validate(content []byte) []FieldError {
	switch k {
	case KindEmail:
		_, errs := ParseDraft(content)
		return errs
	case KindJSON:
		if !json.Valid(content) {
			return []FieldError{{Field: "content", Message: "is not valid JSON"}}
		}
	case KindTable:
		r := csv.NewReader(bytes.NewReader(content))
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		for rows := 0; rows <= MaxGridRows; rows++ {
			_, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return []FieldError{{Field: "content", Message: "is not valid CSV: " + err.Error()}}
			}
		}
	}
	return nil
}

// Draft is a parsed email draft: a header block and a Markdown body.
type Draft struct {
	From      string   `json:"from"`
	To        []string `json:"to"`
	Cc        []string `json:"cc,omitempty"`
	Subject   string   `json:"subject"`
	InReplyTo string   `json:"in_reply_to,omitempty"`
	Body      string   `json:"body"`
}

// FromTrebi is the From value of a draft that the native channel sends.
const FromTrebi = "trebi"

type draftHeader struct {
	Kind      string   `yaml:"kind"`
	From      string   `yaml:"from"`
	To        addrList `yaml:"to"`
	Cc        addrList `yaml:"cc"`
	Subject   string   `yaml:"subject"`
	InReplyTo string   `yaml:"in_reply_to"`
}

// addrList takes one address, a comma list, or a YAML list.
type addrList []string

func (a *addrList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		for _, p := range strings.Split(n.Value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				*a = append(*a, p)
			}
		}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*a = list
	return nil
}

var (
	connRefRE   = regexp.MustCompile(`^([a-z0-9][a-z0-9-]{0,39}|con_[a-z0-9]+)$`)
	messageIDRE = regexp.MustCompile(`^<[^<>@\s]+@[^<>@\s]+>$`)
)

// ParseDraft parses and checks an email draft. Each error names its field.
func ParseDraft(content []byte) (Draft, []FieldError) {
	header, body, ok := splitHeader(content)
	if !ok {
		return Draft{}, []FieldError{{Field: "header", Message: "an email draft starts with a --- header block"}}
	}
	var h draftHeader
	if err := yaml.Unmarshal(header, &h); err != nil {
		return Draft{}, []FieldError{{Field: "header", Message: "is not valid YAML: " + err.Error()}}
	}
	d := Draft{From: strings.TrimSpace(h.From), To: h.To, Cc: h.Cc, Subject: strings.TrimSpace(h.Subject),
		InReplyTo: strings.TrimSpace(h.InReplyTo), Body: body}
	if d.From == "" {
		d.From = FromTrebi
	}
	var errs []FieldError
	if h.Kind != string(KindEmail) {
		errs = append(errs, FieldError{Field: "kind", Message: "must be email"})
	}
	if d.From != FromTrebi && !connRefRE.MatchString(d.From) {
		errs = append(errs, FieldError{Field: "from", Message: "must be trebi or the name of a mailbox connection"})
	}
	if len(d.To) == 0 {
		errs = append(errs, FieldError{Field: "to", Message: "is required"})
	}
	errs = append(errs, checkAddrs("to", d.To)...)
	errs = append(errs, checkAddrs("cc", d.Cc)...)
	if d.Subject == "" {
		errs = append(errs, FieldError{Field: "subject", Message: "is required"})
	}
	if d.InReplyTo != "" && !messageIDRE.MatchString(d.InReplyTo) {
		errs = append(errs, FieldError{Field: "in_reply_to", Message: "must be a Message-ID such as <id@host>"})
	}
	return d, errs
}

func invalid(field, format string, args ...any) *InvalidError {
	return &InvalidError{Fields: []FieldError{{Field: field, Message: fmt.Sprintf(format, args...)}}}
}

// splitHeader returns the YAML header block and the body of a draft.
func splitHeader(content []byte) (header []byte, body string, ok bool) {
	s := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, s, false
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return nil, s, false
	}
	rest := s[4+end+4:]
	rest = strings.TrimPrefix(rest, "\n")
	return []byte(s[4 : 4+end]), rest, true
}

func isEmailDraft(head []byte) bool {
	header, _, ok := splitHeader(head)
	if !ok {
		return false
	}
	var h struct {
		Kind string `yaml:"kind"`
	}
	return yaml.Unmarshal(header, &h) == nil && h.Kind == string(KindEmail)
}

func checkAddrs(field string, list []string) []FieldError {
	var errs []FieldError
	for _, a := range list {
		if _, err := mail.ParseAddress(a); err != nil {
			errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf("%q is not a valid address", a)})
		}
	}
	return errs
}
