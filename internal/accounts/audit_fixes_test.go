package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckProviderAccessUsesClaudeTokenURL(t *testing.T) {
	dataDir := t.TempDir()
	accountDir := filepath.Join(dataDir, "account-1")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Expired Claude access token forces CheckProviderAccess to refresh it.
	auth := map[string]any{
		"provider":        "claude",
		"access_token":    "sk-ant-oat01-old",
		"refresh_token":   "sk-ant-ort01-secret",
		"expires_at":      time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"account_id":      "acct-1",
		"organization_id": "org-1",
		"email":           "claude@example.com",
		"plan":            "Max 20x",
	}
	raw, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(accountDir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var tokenHits atomic.Int32
	var usageHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			tokenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat01-new","refresh_token":"sk-ant-ort01-new","expires_in":3600}`))
		case "/api/oauth/usage":
			usageHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"five_hour":{"utilization":10,"resets_at":"2026-11-24T00:00:00Z"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	origin, _ := url.Parse(server.URL)

	monitor := NewMonitor(dataDir, origin, "", server.Client(), nil)
	monitor.SetClaudeUpstreamOrigin(origin)
	monitor.SetClaudeTokenOrigin(origin)

	if err := monitor.CheckProviderAccess(context.Background()); err != nil {
		t.Fatalf("CheckProviderAccess failed: %v", err)
	}
	if tokenHits.Load() != 1 {
		t.Fatalf("expected 1 hit to custom Claude token URL, got %d", tokenHits.Load())
	}
	if usageHits.Load() != 1 {
		t.Fatalf("expected 1 hit to Claude usage URL, got %d", usageHits.Load())
	}
}

func TestClearClaudeRestrictionsFromSampleMatchesClaimsAndFamily(t *testing.T) {
	earlier := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	fact := &AccountRoutingFact{
		ClaudeRestrictions: map[string]ClaudeRestriction{
			"":       {ObservedAt: earlier, ResetsAt: now.Add(time.Hour), Claim: "five_hour"},
			"sonnet": {ObservedAt: earlier, ResetsAt: now.Add(24 * time.Hour), Claim: "seven_day_sonnet"},
		},
	}
	sample := AllowanceSample{
		ObservedAt: now,
		Windows: []ReportedWindow{
			{ObservedAt: now, DurationSeconds: 5 * 3600, UsedPercent: 40, ResetsAt: now.Add(time.Hour)},
			{ObservedAt: now, DurationSeconds: 7 * 86400, UsedPercent: 20, ResetsAt: now.Add(24 * time.Hour), ModelID: "seven_day_sonnet", ModelName: "Sonnet"},
		},
	}
	clearClaudeRestrictionsFromSample(fact, sample)
	for scope, r := range fact.ClaudeRestrictions {
		if !r.ResetsAt.IsZero() {
			t.Fatalf("expected restriction %q cleared, still active: %+v", scope, r)
		}
	}
}

func TestIDTokenChatGPTAccountIDPrefersAuthNamespace(t *testing.T) {
	claims := map[string]any{
		"chatgpt_account_id": "top-level-wrong",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "namespaced-right",
		},
	}
	payload, _ := json.Marshal(claims)
	jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	if got := idTokenChatGPTAccountID(jwt); got != "namespaced-right" {
		t.Fatalf("idTokenChatGPTAccountID = %q, want namespaced-right", got)
	}
}

func TestPendingResetExpectedIgnoresRemovedOrStaleAccount(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	pending := pendingReset{
		Folder:    "account-2",
		AccountID: "acct-gone",
	}
	raw, _ := json.Marshal(pending)
	pendingPath := filepath.Join(dataDir, "pending-reset.json")
	if err := os.WriteFile(pendingPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	readings := []codexAccountReading{{account: PoolAccount{Folder: "account-1", AccountID: "acct-present"}}}
	if pendingResetExpected(dataDir, readings, now) {
		t.Fatal("expected false when pending reset account is absent from pool")
	}
	readings = append(readings, codexAccountReading{account: PoolAccount{Folder: "account-2", AccountID: "acct-gone"}, signedOut: true})
	if pendingResetExpected(dataDir, readings, now) {
		t.Fatal("expected false when pending reset account is signed out")
	}
	readings[1].signedOut = false
	if !pendingResetExpected(dataDir, readings, now) {
		t.Fatal("expected true when pending reset account is present, signed in, and recent")
	}
	if pendingResetExpected(dataDir, readings, now.Add(readingLagWarning+time.Minute)) {
		t.Fatal("expected false when pending reset is older than readingLagWarning")
	}
}
