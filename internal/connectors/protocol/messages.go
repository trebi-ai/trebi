package protocol

import (
	"encoding/json"
	"time"
)

// Version is the protocol name in initialize.
const Version = "trebi-connector/1"

// Methods of Part D. The daemon sends requests; the adapter sends the
// notifications event, status, auth/step, and auth/done.
const (
	MethodInitialize  = "initialize"
	MethodInitialized = "initialized"
	MethodPing        = "ping"
	MethodShutdown    = "shutdown"

	MethodEvent  = "event"
	MethodStatus = "status"

	MethodAuthStatus = "auth/status"
	MethodAuthBegin  = "auth/begin"
	MethodAuthSubmit = "auth/submit"
	MethodAuthCancel = "auth/cancel"
	MethodAuthLogout = "auth/logout"
	MethodAuthStep   = "auth/step"
	MethodAuthDone   = "auth/done"

	MethodMessagesSend    = "messages/send"
	MethodMessagesEdit    = "messages/edit"
	MethodMessagesHistory = "messages/history"
	MethodMessagesSeen    = "messages/seen"
	MethodEventsReplay    = "events/replay"
	MethodRoomsList       = "rooms/list"
	MethodRoomsGet        = "rooms/get"
	MethodRoomsOpen       = "rooms/open"
	MethodThreadsList     = "threads/list"
	MethodThreadsCreate   = "threads/create"
	MethodTyping          = "typing"
	MethodReactionsAdd    = "reactions/add"
)

// Adapter states of the status notification (Part D.4).
const (
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateAuthRequired = "auth_required"
	StateAuthPending  = "auth_pending"
	StateRateLimited  = "rate_limited"
	StateError        = "error"
)

// Reasons of auth_required.
const (
	ReasonNone      = "none"
	ReasonExpired   = "expired"
	ReasonRevoked   = "revoked"
	ReasonLoggedOut = "logged_out"
)

// Login step kinds.
const (
	StepQR         = "qr"
	StepDeviceCode = "device_code"
	StepURL        = "url"
	StepInput      = "input"
	StepWait       = "wait"
)

// Room kinds.
const (
	RoomDM      = "dm"
	RoomGroup   = "group"
	RoomChannel = "channel"
	RoomMailbox = "mailbox"
)

// Request timeouts of Part D.1. A zero timeout means none.
const (
	DefaultTimeout = 10 * time.Second
	SendTimeout    = 30 * time.Second
	ReplayTimeout  = 60 * time.Second
)

// Empty is the params or result of a message with no fields.
type Empty struct{}

// DaemonInfo names the daemon build.
type DaemonInfo struct {
	Version string `json:"version"`
}

// InstanceInfo names the instance the adapter serves.
type InstanceInfo struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Workspace string `json:"workspace,omitempty"`
}

// InitializeParams starts the session. LastEventID is the connector cursor
// of the instance, when there is one.
type InitializeParams struct {
	Protocol    string       `json:"protocol"`
	Daemon      DaemonInfo   `json:"daemon"`
	Instance    InstanceInfo `json:"instance"`
	LastEventID string       `json:"cursor,omitempty"`
}

// AdapterInfo names the adapter build.
type AdapterInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Account is the account the adapter is logged in to.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// EventDecl is one event type the adapter emits.
type EventDecl struct {
	Type string `json:"type"`
}

// Limits bound one message.
type Limits struct {
	MaxText int      `json:"max_text,omitempty"`
	Formats []string `json:"formats,omitempty"`
}

// InitializeResult is what the process delivers.
type InitializeResult struct {
	Protocol string      `json:"protocol"`
	Adapter  AdapterInfo `json:"adapter"`
	Account  *Account    `json:"account,omitempty"`
	Events   []EventDecl `json:"events"`
	Features []string    `json:"features"`
	Limits   Limits      `json:"limits"`
	Login    []string    `json:"login"`
}

// Room is one conversation place: a channel, a group, a DM, or a mailbox.
type Room struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Parent         string `json:"parent,omitempty"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
}

// Thread is one conversation line in a room.
type Thread struct {
	ID             string   `json:"id"`
	Title          string   `json:"title,omitempty"`
	LastActivityAt string   `json:"last_activity_at,omitempty"`
	Participants   []string `json:"participants,omitempty"`
}

// Sender is who wrote an event. ID is the identity for trigger allowlists.
type Sender struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Self bool   `json:"self,omitempty"`
}

// Attachment is one file on a message.
type Attachment struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size,omitempty"`
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
}

// Event is one inbound event (Part D.3). ID is the dedupe key and the
// connector cursor.
type Event struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	TS          string          `json:"ts"`
	Room        *Room           `json:"room"`
	Thread      *Thread         `json:"thread"`
	Sender      *Sender         `json:"sender"`
	Text        string          `json:"text,omitempty"`
	ReplyTo     string          `json:"reply_to,omitempty"`
	Attachments []Attachment    `json:"attachments,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

// Status is the status notification.
type Status struct {
	State   string   `json:"state"`
	Reason  string   `json:"reason,omitempty"`
	Message string   `json:"message,omitempty"`
	Account *Account `json:"account,omitempty"`
}

// AuthStatusResult answers auth/status.
type AuthStatusResult struct {
	State   string   `json:"state"`
	Account *Account `json:"account,omitempty"`
}

// StepField is one field of an input step.
type StepField struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Secret bool   `json:"secret,omitempty"`
	Help   string `json:"help,omitempty"`
}

// Step is one login step. The fields depend on the kind.
type Step struct {
	Kind      string      `json:"kind"`
	Data      string      `json:"data,omitempty"`
	URL       string      `json:"url,omitempty"`
	Code      string      `json:"code,omitempty"`
	Fields    []StepField `json:"fields,omitempty"`
	Message   string      `json:"message,omitempty"`
	ExpiresAt string      `json:"expires_at,omitempty"`
}

// AuthBeginParams starts a login flow. An empty kind lets the adapter pick.
type AuthBeginParams struct {
	Kind string `json:"kind,omitempty"`
}

// AuthBeginResult is the first step of the flow.
type AuthBeginResult struct {
	FlowID string `json:"flow_id"`
	Step   Step   `json:"step"`
}

// AuthSubmitParams answers an input step.
type AuthSubmitParams struct {
	FlowID string            `json:"flow_id"`
	Fields map[string]string `json:"fields"`
}

// AuthSubmitResult carries the next step, when the adapter knows it now.
type AuthSubmitResult struct {
	Step *Step `json:"step,omitempty"`
}

// AuthFlowParams names a flow (auth/cancel).
type AuthFlowParams struct {
	FlowID string `json:"flow_id"`
}

// AuthStepParams is the auth/step notification.
type AuthStepParams struct {
	FlowID string `json:"flow_id"`
	Step   Step   `json:"step"`
}

// AuthDoneParams is the auth/done notification.
type AuthDoneParams struct {
	FlowID  string   `json:"flow_id"`
	OK      bool     `json:"ok"`
	Account *Account `json:"account,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// SendParams sends one message. Key is the outbox row id: the adapter drops
// a second send with the same key.
type SendParams struct {
	Room        string       `json:"room"`
	Thread      string       `json:"thread,omitempty"`
	Text        string       `json:"text"`
	Format      string       `json:"format"`
	ReplyTo     string       `json:"reply_to,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Key         string       `json:"key"`
}

// SendResult names the sent message.
type SendResult struct {
	MessageID string `json:"message_id"`
	Thread    string `json:"thread,omitempty"`
}

// EditParams edits a sent message.
type EditParams struct {
	Room      string `json:"room"`
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

// HistoryParams reads older messages of a room.
type HistoryParams struct {
	Room   string `json:"room"`
	Thread string `json:"thread,omitempty"`
	Before string `json:"before,omitempty"`
	Limit  int    `json:"limit"`
}

// EventPage is a page of events.
type EventPage struct {
	Events []Event `json:"events"`
	Next   string  `json:"next,omitempty"`
}

// ReplayParams asks for the events after a connector cursor.
type ReplayParams struct {
	After string `json:"after"`
	Limit int    `json:"limit"`
}

// ReplayResult is the replay. Complete false means the cursor is too old.
type ReplayResult struct {
	Events   []Event `json:"events"`
	Complete bool    `json:"complete"`
}

// RoomsListParams searches rooms. Page is the opaque page token.
type RoomsListParams struct {
	Query string `json:"query,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Page  string `json:"cursor,omitempty"`
	Limit int    `json:"limit"`
}

// RoomPage is a page of rooms.
type RoomPage struct {
	Rooms []Room `json:"rooms"`
	Next  string `json:"next,omitempty"`
}

// RoomParams names a room (rooms/get).
type RoomParams struct {
	Room string `json:"room"`
}

// RoomOpenParams opens the DM room with a user.
type RoomOpenParams struct {
	User string `json:"user"`
}

// RoomResult carries one room.
type RoomResult struct {
	Room Room `json:"room"`
}

// ThreadsListParams lists the threads of a room.
type ThreadsListParams struct {
	Room  string `json:"room"`
	Page  string `json:"cursor,omitempty"`
	Limit int    `json:"limit"`
}

// ThreadPage is a page of threads.
type ThreadPage struct {
	Threads []Thread `json:"threads"`
	Next    string   `json:"next,omitempty"`
}

// ThreadCreateParams starts a thread.
type ThreadCreateParams struct {
	Room        string `json:"room"`
	Title       string `json:"title,omitempty"`
	FromMessage string `json:"from_message,omitempty"`
}

// ThreadResult carries one thread.
type ThreadResult struct {
	Thread Thread `json:"thread"`
}

// TypingParams shows the typing indicator.
type TypingParams struct {
	Room   string `json:"room"`
	Thread string `json:"thread,omitempty"`
}

// SeenParams marks a message as seen.
type SeenParams struct {
	Room      string `json:"room"`
	MessageID string `json:"message_id"`
}

// ReactionParams adds a reaction.
type ReactionParams struct {
	Room      string `json:"room"`
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji"`
}

// Timeout returns the request timeout of a method. Zero means none.
func Timeout(method string) time.Duration {
	switch method {
	case MethodMessagesSend:
		return SendTimeout
	case MethodEventsReplay:
		return ReplayTimeout
	case MethodAuthBegin:
		return 0
	}
	return DefaultTimeout
}

// FeatureOf returns the channel feature a method needs, or "" for a core
// method.
func FeatureOf(method string) string {
	switch method {
	case MethodMessagesEdit:
		return "edit"
	case MethodMessagesHistory:
		return "history"
	case MethodEventsReplay:
		return "replay"
	case MethodRoomsList, MethodRoomsGet:
		return "rooms.list"
	case MethodRoomsOpen:
		return "rooms.open"
	case MethodThreadsList:
		return "threads"
	case MethodThreadsCreate:
		return "threads.create"
	case MethodTyping:
		return "typing"
	case MethodMessagesSeen:
		return "seen"
	case MethodReactionsAdd:
		return "reactions"
	}
	return ""
}
