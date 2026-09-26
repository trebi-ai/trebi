package assistant

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// MintSessionToken returns 32 random bytes as hex and their SHA-256 hex hash
// (plan 2026-09-26 D.8). Only the hash is stored; the plaintext travels to
// the session process environment or the caller.
func MintSessionToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("mint session token: %w", err)
	}
	token = hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// HashSessionToken is the deterministic verifier used by the API middleware.
func HashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
