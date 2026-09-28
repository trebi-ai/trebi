// Package notify owns the notification route model (plan 2026-09-28 Part C).
package notify

// Step is one route entry. Via is "email" or a connector name. To is an email
// address, the token "contact", the token "admins", or a channel ref.
type Step struct {
	Via string `yaml:"via" json:"via"`
	To  string `yaml:"to,omitempty" json:"to,omitempty"`
}
