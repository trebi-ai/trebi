// Package cloudlink holds the published hosted-link wire shapes.
// The daemon client that speaks this contract is a separate plan.
// A shape change fails TestContractFixtures before either side deploys.
package cloudlink

import "encoding/json"

// ContractV is the wire version every frame carries.
const ContractV = 1

// Hello is the first frame on the socket. The upgrade request carries the signature.
// Caps lists optional daemon features. An absent list means none.
type Hello struct {
	V    int      `json:"v"`
	Type string   `json:"type"`
	Caps []string `json:"caps,omitempty"`
}

// Hello caps.
const (
	// CapMCP tells the cloud that remote MCP is on for this machine.
	CapMCP = "mcp"
	// CapMobile tells the cloud that phone access is on for this machine.
	CapMobile = "mobile"
)

// Heartbeat re-arms the device alarm. Capabilities is set only when the
// summary changed since the last heartbeat on this socket.
type Heartbeat struct {
	V            int           `json:"v"`
	Type         string        `json:"type"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities is the summary of what the machine can run. It holds names
// and states only, never a secret.
type Capabilities struct {
	OS         string           `json:"os"`
	Arch       string           `json:"arch"`
	Version    string           `json:"version"`
	Harnesses  []CapabilityItem `json:"harnesses"`
	Connectors []CapabilityItem `json:"connectors"`
}

// CapabilityItem is one harness or one connection. Type is the connector
// type; a harness has none.
type CapabilityItem struct {
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"`
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
}

// Alert is notification text plus the daemon-minted message id.
// Summary is not a run log.
type Alert struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	Cursor    string `json:"cursor"`
	Kind      string `json:"kind"`
	Summary   string `json:"summary"`
	MessageID string `json:"message_id"`
}

// Ack deletes the ingress buffer through the named id.
type Ack struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	ID   string `json:"id"`
}

// RelayFrame carries opaque bytes of one phone session. The cloud does not decode them.
type RelayFrame struct {
	V     int    `json:"v"`
	Type  string `json:"type"`
	SID   string `json:"sid"`
	Bytes string `json:"bytes"`
}

// RelayOpen tells the daemon that a phone socket opened a session.
type RelayOpen struct {
	V        int    `json:"v"`
	Type     string `json:"type"`
	SID      string `json:"sid"`
	ClientID string `json:"client_id"`
}

// RelayClose ends one phone session. Either side sends it.
type RelayClose struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	SID    string `json:"sid"`
	Reason string `json:"reason"`
}

// ClientRevoked tells the daemon that the account removed a phone.
type ClientRevoked struct {
	V        int    `json:"v"`
	Type     string `json:"type"`
	ClientID string `json:"client_id"`
}

// ClaimMintRequest asks the cloud for a one-time pairing claim.
type ClaimMintRequest struct {
	Uses       int `json:"uses"`
	TTLSeconds int `json:"ttl_seconds"`
}

// Claim is the minted pairing claim.
type Claim struct {
	Claim     string `json:"claim"`
	ExpiresAt string `json:"expires_at"`
}

// PeerEnrollRequest makes this machine a client of another machine of the
// account (POST /v1/clients/peers).
type PeerEnrollRequest struct {
	Claim string `json:"claim"`
	Name  string `json:"name"`
}

// Enrollment is the client id and the client token of a new client.
type Enrollment struct {
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
}

// HelloOK is the fact sheet the daemon caches. Features are a hint, not a grant.
type HelloOK struct {
	V                 int      `json:"v"`
	Type              string   `json:"type"`
	Plan              string   `json:"plan"`
	Features          []string `json:"features"`
	HeartbeatInterval int      `json:"heartbeat_interval"`
	McpURL            string   `json:"mcp_url,omitempty"`
}

// Ingress is one buffered payload. Kind email carries the email contract.
// Kind webhook carries WebhookIngress. There is no decision frame.
type Ingress struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// Entitlement is pushed when the plan changes.
type Entitlement struct {
	V        int      `json:"v"`
	Type     string   `json:"type"`
	Plan     string   `json:"plan"`
	Features []string `json:"features"`
}

// WebhookIngress is the raw hook the daemon verifies. The cloud does not parse Body into a job.
// Handshake is true when the cloud answered the request from a handshake rule.
type WebhookIngress struct {
	V           int               `json:"v"`
	ID          string            `json:"id"`
	HookID      string            `json:"hook_id,omitempty"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Query       string            `json:"query,omitempty"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	ReceivedAt  string            `json:"received_at"`
	Handshake   bool              `json:"handshake,omitempty"`
}

// McpRequest is one MCP HTTP POST that the cloud forwards. The cloud does
// not store or log Body. Headers keys are lowercase.
type McpRequest struct {
	V       int               `json:"v"`
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Client  string            `json:"client"`
	Scope   []string          `json:"scope"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// McpResponse is the daemon reply to the McpRequest with the same ID.
type McpResponse struct {
	V       int               `json:"v"`
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// LoginReturn is one OAuth return page hit on the cloud. The state starts
// with the device id. The cloud does not store or log code.
type LoginReturn struct {
	V                int    `json:"v"`
	Type             string `json:"type"`
	ID               string `json:"id"`
	State            string `json:"state"`
	Code             string `json:"code,omitempty"`
	Iss              string `json:"iss,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// LoginResult is the daemon reply to the LoginReturn with the same ID.
type LoginResult struct {
	V       int    `json:"v"`
	Type    string `json:"type"`
	ID      string `json:"id"`
	Status  string `json:"status"` // done | failed | gone
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
}

// Login result statuses.
const (
	LoginDone   = "done"
	LoginFailed = "failed"
	LoginGone   = "gone"
)
