package agentcli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// grokAccount is one entry of a grok 1.0.40 auth.json.
type grokAccount struct {
	Email        string `json:"email"`
	ExpiresAt    string `json:"expires_at"`
	RefreshToken string `json:"refresh_token"`
}

// readGrokAuth returns the entries of a grok auth.json, keyed by "<issuer>::<client>".
func readGrokAuth(path string) (map[string]grokAccount, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("grok auth.json: %w", err)
	}
	out := map[string]grokAccount{}
	for k, v := range raw {
		var a grokAccount
		if json.Unmarshal(v, &a) == nil {
			out[k] = a
		}
	}
	return out, nil
}

// firstGrokAccount returns the entry with the first key in sort order.
func firstGrokAccount(path string) (grokAccount, bool) {
	accts, err := readGrokAuth(path)
	if err != nil || len(accts) == 0 {
		return grokAccount{}, false
	}
	keys := make([]string, 0, len(accts))
	for k := range accts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return accts[keys[0]], true
}
