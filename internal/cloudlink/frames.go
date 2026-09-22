// Package cloudlink holds the published hosted-link wire shapes.
// The daemon client that speaks this contract is a separate plan.
// A shape change fails TestContractFixtures before either side deploys.
package cloudlink

import "encoding/json"

// ContractV is the wire version every frame carries.
const ContractV = 1

// Hello is the first frame on the socket. The upgrade request carries the signature.
type Hello struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// Heartbeat re-arms the device alarm.
type Heartbeat struct {
	V    int    `json:"v"`
	Type string `json:"type"`
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

// RelayFrame carries opaque bytes. The cloud does not decode them.
type RelayFrame struct {
	V     int    `json:"v"`
	Type  string `json:"type"`
	Bytes string `json:"bytes"`
}

// HelloOK is the fact sheet the daemon caches. Features are a hint, not a grant.
type HelloOK struct {
	V                 int      `json:"v"`
	Type              string   `json:"type"`
	Plan              string   `json:"plan"`
	Features          []string `json:"features"`
	HeartbeatInterval int      `json:"heartbeat_interval"`
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
type WebhookIngress struct {
	V           int               `json:"v"`
	ID          string            `json:"id"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	ReceivedAt  string            `json:"received_at"`
}
