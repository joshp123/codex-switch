package relay

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

func TestClaudeConversationIdentityKeepsPiPinAndCodeForkAffinity(t *testing.T) {
	request := &http.Request{Header: make(http.Header)}
	first := []byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"text","text":"First turn","cache_control":{"type":"ephemeral"}}]}]}`)
	continuation := []byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"text","text":"First turn"}]},{"role":"assistant","content":"Reply"},{"role":"user","content":"Second turn"}]}`)
	firstPrefixes := claudeConversationPrefixes(first)
	continuedPrefixes := claudeConversationPrefixes(continuation)
	firstID := claudeSessionForRequest(request, firstPrefixes)
	continuedID := claudeSessionForRequest(request, continuedPrefixes)
	if firstID == "" || firstID != continuedID {
		t.Fatal("Pi continuation lost its conversation identity as cache markers moved")
	}
	database, err := store.OpenStoreDatabase(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.CloseStoreDatabase()
	rememberTestAccounts(t, database, "account-1", "account-2")
	if _, err := database.PinTaskAccountIfAbsent(store.TaskAccountPin{TaskID: firstID, AccountKey: "account-1", PinnedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if pin, found, err := database.ReadTaskAccountPin(continuedID); err != nil || !found || pin.AccountKey != "account-1" {
		t.Fatalf("Pi continuation lost its account pin: found=%v, error=%v", found, err)
	}
	request.Header.Set("X-Claude-Code-Session-Id", "parent")
	parentID := claudeSessionForRequest(request, firstPrefixes)
	if _, err := database.PinTaskAccountIfAbsent(store.TaskAccountPin{TaskID: parentID, AccountKey: "account-2", PinnedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := database.RememberPromptCachePrefix(parentID, firstPrefixes[0]); err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Claude-Code-Session-Id", "fork")
	forkID := claudeSessionForRequest(request, continuedPrefixes)
	parent, err := database.ReadPromptCacheParent(forkID, continuedPrefixes)
	if err != nil || parent != parentID || forkID == parentID {
		t.Fatalf("Claude Code fork lost its cache parent: error=%v", err)
	}
}
