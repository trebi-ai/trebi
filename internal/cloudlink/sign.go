package cloudlink

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// The four signed headers CONTRACT.md defines.
const (
	headerDevice    = "X-Trebi-Device"
	headerTimestamp = "X-Trebi-Timestamp"
	headerNonce     = "X-Trebi-Nonce"
	headerSignature = "X-Trebi-Signature"
)

// SignRequest signs one call with the canonical payload and sets the four
// headers. The nonce is a fresh ULID per call: no CR, LF, or NUL.
func SignRequest(req *http.Request, id Identity, now time.Time) {
	timestamp := strconv.FormatInt(now.UnixMilli(), 10)
	nonce := ulid.Make().String()
	req.Header.Set(headerDevice, id.DeviceID())
	req.Header.Set(headerTimestamp, timestamp)
	req.Header.Set(headerNonce, nonce)
	payload := signPayload(timestamp, nonce, req.Method, req.URL.Path)
	req.Header.Set(headerSignature, id.Sign([]byte(payload)))
}

// signPayload builds the canonical string the worker verifies:
// timestamp LF nonce LF METHOD LF pathname. The method is uppercase and
// the pathname carries no query and no host.
func signPayload(timestamp, nonce, method, path string) string {
	return timestamp + "\n" + nonce + "\n" + strings.ToUpper(method) + "\n" + path
}
