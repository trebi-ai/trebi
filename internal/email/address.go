package email

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// Address grammar (plan 2026-09-19 Part D):
//
//	<slug>-<token>            workspace assistant
//	<slug>-<token>+<tag>      agent <tag>
//
// The token is 8 Crockford base32 characters (40 bits) from crypto/rand.
// The slug is frozen at mint; a later rename does not change the address.
const (
	tokenLen  = 8
	slugMax   = 24
	agentMax  = 24
	localMax  = 64 // RFC 5321 octets; worst case 24+8+24+2 = 58
	crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// mintToken returns 8 Crockford base32 characters of crypto/rand entropy.
func mintToken() (string, error) {
	b := make([]byte, 5) // 40 bits → 8 base32 chars, no padding
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	return encodeCrockford(b), nil
}

// encodeCrockford turns 5 bytes into 8 Crockford characters.
func encodeCrockford(b []byte) string {
	n := uint64(b[0])<<32 | uint64(b[1])<<24 | uint64(b[2])<<16 | uint64(b[3])<<8 | uint64(b[4])
	var sb strings.Builder
	sb.Grow(tokenLen)
	for shift := 35; shift >= 0; shift -= 5 {
		sb.WriteByte(crockford[(n>>uint(shift))&0x1f])
	}
	return sb.String()
}

// slugify derives the reading-aid part of the local part: lowercase,
// [a-z0-9-], collapsed dashes, capped at 24. The token alone identifies
// the workspace, so a slug collision between two workspaces is harmless.
func slugify(name string) string {
	var sb strings.Builder
	prevDash := true // also trims leading dashes
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				sb.WriteByte('-')
				prevDash = true
			}
		}
		if sb.Len() >= slugMax {
			break
		}
	}
	out := strings.Trim(sb.String(), "-")
	if len(out) > slugMax {
		out = out[:slugMax]
	}
	if out == "" {
		out = "ws"
	}
	return out
}

// localPart builds the local part and enforces the RFC 5321 budget. The
// agent tag cap is enforced here so agent creation fails with a clear
// error instead of a silent truncation.
func localPart(slug, token, tag string) (string, error) {
	if len(tag) > agentMax {
		return "", fmt.Errorf("agent name %q is %d characters; the email tag cap is %d", tag, len(tag), agentMax)
	}
	lp := slug + "-" + token
	if tag != "" {
		lp += "+" + tag
	}
	if len(lp) > localMax {
		return "", fmt.Errorf("local part %q exceeds %d octets", lp, localMax)
	}
	return lp, nil
}

// tagAddress returns the agent subaddress base+tag for one agent.
func tagAddress(a Address, agent string) (string, error) {
	slug, token, err := splitLocalPart(a.LocalPart)
	if err != nil {
		return "", err
	}
	return localPart(slug, token, agent)
}

// splitLocalPart splits <slug>-<token> into slug and token.
func splitLocalPart(lp string) (slug, token string, err error) {
	i := strings.LastIndex(lp, "-")
	if i <= 0 || i == len(lp)-1 {
		return "", "", fmt.Errorf("local part %q is not <slug>-<token>", lp)
	}
	return lp[:i], lp[i+1:], nil
}

// maskLocalPart hides the token, keeps the reading aids: slug and tag.
// ellis-x7k2qm4p+research → ellis-********+research
func maskLocalPart(lp string) string {
	base, tag, _ := strings.Cut(lp, "+")
	if i := strings.LastIndex(base, "-"); i >= 0 {
		base = base[:i+1] + "********"
	} else {
		base = "********"
	}
	if tag != "" {
		return base + "+" + tag
	}
	return base
}
