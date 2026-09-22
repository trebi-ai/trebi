// Package entitle holds the cloud grant cached from the hosted side. The
// daemon never computes a grant: it reads plan and features as the worker
// sent them and answers "is this hosted feature granted right now". The
// local license answers a different question and stays separate.
package entitle

import "time"

// Feature is one hosted capability the cloud plan grants.
type Feature string

// The four feature names. They match the hosted entitlement table exactly;
// entitle_test pins them to the vendored contract.
const (
	Watchdog      Feature = "watchdog"
	RemoteAccess  Feature = "remote_access"
	HostedIngress Feature = "hosted_ingress"
	Backup        Feature = "backup"
)

// State is the grant cached in the cloud_link row. It gates what the UI
// shows; every gate that matters is a signed call the worker re-checks.
type State struct {
	Linked      bool
	Plan        string
	Features    []Feature
	RefreshedAt time.Time
}

// Has reports whether the grant includes f.
func (s State) Has(f Feature) bool {
	for _, have := range s.Features {
		if have == f {
			return true
		}
	}
	return false
}
