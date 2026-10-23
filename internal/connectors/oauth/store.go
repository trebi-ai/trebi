package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/trebi-ai/trebi/internal/secrets"
)

// ErrNoRecord is a connection with no stored OAuth state.
var ErrNoRecord = errors.New("no OAuth record")

// Registration kinds: how Trebi got its client at the auth server.
const (
	RegCIMD   = "cimd"    // a client ID metadata document URL is the client id
	RegDCR    = "dcr"     // dynamic client registration (RFC 7591)
	RegOwnApp = "own_app" // the user's own app: client id and secret
	RegAuto   = "auto"    // pick in the order own app, CIMD, DCR
)

// Server is the discovered metadata of one online MCP server and its auth
// server.
type Server struct {
	Resource    string   `json:"resource"` // canonical server URL (RFC 8707)
	Issuer      string   `json:"issuer"`
	AuthURL     string   `json:"authorization_endpoint"`
	TokenURL    string   `json:"token_endpoint"`
	RegisterURL string   `json:"registration_endpoint,omitempty"`
	RevokeURL   string   `json:"revocation_endpoint,omitempty"`
	Scopes      []string `json:"scopes,omitempty"` // scopes the server asks for
	CIMD        bool     `json:"cimd,omitempty"`
	IssParam    bool     `json:"iss_param,omitempty"`
	AuthMethods []string `json:"auth_methods,omitempty"` // token endpoint auth methods
}

// Registration is the client of Trebi at one auth server.
type Registration struct {
	Kind         string    `json:"kind"`
	ID           string    `json:"client_id"`
	Secret       string    `json:"client_secret,omitempty"`
	Issuer       string    `json:"issuer"`
	RedirectURIs []string  `json:"redirect_uris,omitempty"` // registered return addresses (DCR)
	SecretExpiry time.Time `json:"secret_expires_at,omitzero"`
}

// Token is one grant. An empty RefreshToken cannot renew.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
	Scopes       []string  `json:"scopes,omitempty"`
	ObtainedAt   time.Time `json:"obtained_at"`
}

// Record is the OAuth state of one connection. A logout keeps Server and
// Client and drops Token, so the next login needs no new registration.
type Record struct {
	Server *Server       `json:"server,omitempty"`
	Client *Registration `json:"client,omitempty"`
	Token  *Token        `json:"token,omitempty"`
}

// Store keeps the OAuth state of a connection. Load returns ErrNoRecord
// for a connection with none.
type Store interface {
	Load(conn string) (Record, error)
	Save(conn string, r Record) error
	Delete(conn string) error
}

// SecretStore keeps the token and server under oauth.<connection id>, and
// the client under oauth.<connection id>.client, in the secrets directory.
type SecretStore struct{}

// Load reads both parts. A missing part stays nil.
func (s SecretStore) Load(conn string) (Record, error) {
	grantName, clientName := s.names(conn)
	var r Record
	found := false
	for _, part := range []struct {
		name string
		into any
	}{{grantName, &r}, {clientName, &r.Client}} {
		v, err := secrets.Get(part.name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Record{}, err
		}
		if err := json.Unmarshal([]byte(v), part.into); err != nil {
			return Record{}, fmt.Errorf("read %s: %w", part.name, err)
		}
		found = true
	}
	if !found {
		return Record{}, ErrNoRecord
	}
	return r, nil
}

// Save writes both parts. A nil client removes its part.
func (s SecretStore) Save(conn string, r Record) error {
	grantName, clientName := s.names(conn)
	client := r.Client
	r.Client = nil
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := secrets.Set(grantName, string(b)); err != nil {
		return err
	}
	if client == nil {
		return secrets.Delete(clientName)
	}
	cb, err := json.Marshal(client)
	if err != nil {
		return err
	}
	return secrets.Set(clientName, string(cb))
}

// Delete removes both parts.
func (s SecretStore) Delete(conn string) error {
	grantName, clientName := s.names(conn)
	return errors.Join(secrets.Delete(grantName), secrets.Delete(clientName))
}

func (SecretStore) names(conn string) (grant, client string) {
	return secrets.ReservedPrefix + conn, secrets.ReservedPrefix + conn + ".client"
}
