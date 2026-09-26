package assistant

// Exported wire errors. The /v1 layer maps them to HTTP statuses; the
// manager and session raise them wrapped with detail.
var (
	ErrValidation        = errValidation
	ErrForbidden         = errForbidden
	ErrConflict          = errConflict
	ErrUnavailable       = errUnavailable
	ErrNoSnapshot        = errNoSnapshot
	ErrNoLive            = errNoLive
	ErrPermissionUnknown = errPermissionUnknown
	ErrDecisionInvalid   = errDecisionInvalid
)
