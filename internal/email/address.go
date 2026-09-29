package email

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// Address grammar:
//
//	<handle>-<token>            workspace assistant
//	<handle>-<token>+<tag>      agent <tag>
//
// The handle is the workspace handle; it is fixed while the address lives.
// The token is 6 lowercase Crockford base32 characters from crypto/rand.
const (
	tokenLen  = 6
	agentMax  = 24
	localMax  = 64 // RFC 5321 octets; worst case 20+6+24+2 = 52
	crockford = "0123456789abcdefghjkmnpqrstvwxyz"
)

// MailboxRoom is the channel room of one mailbox: the workspace id, or
// <workspace>+<agent> for an agent subaddress. It survives address rotation.
func MailboxRoom(workspaceID, agent string) string {
	if agent == "" {
		return workspaceID
	}
	return workspaceID + "+" + agent
}

// mintToken returns tokenLen Crockford characters of crypto/rand entropy.
// 256 is a multiple of 32, so the low five bits of a byte are uniform.
func mintToken() (string, error) {
	b := make([]byte, tokenLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	for i := range b {
		b[i] = crockford[b[i]&0x1f]
	}
	return string(b), nil
}

// localPart builds the local part and enforces the RFC 5321 budget. The
// agent tag cap is enforced here so agent creation fails with a clear
// error instead of a silent truncation.
func localPart(handle, token, tag string) (string, error) {
	if len(tag) > agentMax {
		return "", fmt.Errorf("agent name %q is %d characters; the email tag cap is %d", tag, len(tag), agentMax)
	}
	lp := handle + "-" + token
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
	handle, token, err := splitLocalPart(a.LocalPart)
	if err != nil {
		return "", err
	}
	return localPart(handle, token, agent)
}

// splitLocalPart splits <handle>-<token> into handle and token.
func splitLocalPart(lp string) (handle, token string, err error) {
	i := strings.LastIndex(lp, "-")
	if i <= 0 || i == len(lp)-1 {
		return "", "", fmt.Errorf("local part %q is not <handle>-<token>", lp)
	}
	return lp[:i], lp[i+1:], nil
}

// maskLocalPart hides the token, keeps the reading aids: handle and tag.
// ellis-x7k2qm+research → ellis-******+research
func maskLocalPart(lp string) string {
	base, tag, _ := strings.Cut(lp, "+")
	if i := strings.LastIndex(base, "-"); i >= 0 {
		base = base[:i+1] + "******"
	} else {
		base = "******"
	}
	if tag != "" {
		return base + "+" + tag
	}
	return base
}
