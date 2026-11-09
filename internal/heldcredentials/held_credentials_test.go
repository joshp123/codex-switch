package heldcredentials

import (
	"strings"
	"testing"

	"github.com/joshp123/codex-switch/internal/menupb"
)

// A provider may quote a token whole, cut short by a shortened quote, or next
// to another token from the same issuer, which shares its start. No part of
// either token longer than its public start may remain.
func TestRemoveLeavesNoPartOfAToken(t *testing.T) {
	header := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9."
	access := header + "eyJzdWIiOiJhY2Nlc3MifQ." + strings.Repeat("a", 64)
	identity := header + "eyJzdWIiOiJpZGVudGl0eSJ9." + strings.Repeat("i", 64)
	Remember(access, identity, "short")
	for name, text := range map[string]string{
		"whole":          "rejected Bearer " + access + " for now",
		"cut short":      "rejected Bearer " + access[:40] + "...",
		"shared start":   "id " + identity + " and access " + access,
		"back to back":   access + identity,
		"repeated quote": access + " " + access,
	} {
		removed := Remove(text)
		for _, token := range []string{access, identity} {
			for start := 0; start+minimumLength <= len(token); start++ {
				if piece := token[start : start+minimumLength]; strings.Contains(removed, piece) {
					t.Errorf("%s: %q still holds %q", name, removed, piece)
					break
				}
			}
		}
		if !strings.Contains(removed, replacement) {
			t.Errorf("%s: %q", name, removed)
		}
	}
	if got := Remove("a short word"); got != "a short word" {
		t.Errorf("a value too short to be a credential was removed: %q", got)
	}
}

func TestRemoveFromMessageReachesNestedStrings(t *testing.T) {
	token := "sk-ant-ort01-" + strings.Repeat("r", 80)
	Remember(token)
	snapshot := &menupb.MenuSnapshot{Accounts: []*menupb.Account{{UnavailableReason: "cannot read its saved login: Refresh token " + token + " not found"}}}
	RemoveFromMessage(snapshot)
	if got := snapshot.Accounts[0].UnavailableReason; strings.Contains(got, token[:minimumLength]) {
		t.Errorf("snapshot still shows the token: %q", got)
	}
}
