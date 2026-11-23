package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

func TestAPIModelPricesIncludeNewOpenAIModels(t *testing.T) {
	u := tokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol", "gpt-6.1-terra"} {
		if price := apiEquivalentPrice(model, harnessStandard, u); price == nil || *price <= 0 {
			t.Fatalf("expected positive price for %s, got %v", model, price)
		}
	}
}

func TestMergeClaudeUsageClearsStaleCacheCreation(t *testing.T) {
	cw1 := int64(100)
	cw2 := int64(250)
	dst := claudeMessageUsage{
		CacheWrite:    &cw1,
		CacheCreation: &claudeCacheCreation{OneHour: cw1},
	}
	mergeClaudeUsage(&dst, claudeMessageUsage{CacheWrite: &cw2})
	if dst.CacheWrite == nil || *dst.CacheWrite != 250 {
		t.Fatalf("CacheWrite = %v, want 250", dst.CacheWrite)
	}
	if dst.CacheCreation != nil {
		t.Fatalf("expected stale CacheCreation cleared, got %+v", dst.CacheCreation)
	}
}

func TestProxyCompletionFallsBackToClaudePricing(t *testing.T) {
	cw := int64(1000)
	rec := store.CompletedResponseRecord{
		TaskID:                "claude:code:s1",
		AccountKey:            "claude:a@example.com",
		ModelSlug:             "claude-sonnet-4-6",
		InputTokens:           5000,
		CachedInputTokens:     1000,
		CacheWriteInputTokens: &cw,
		OutputTokens:          500,
		CompletedAt:           time.Now().UTC(),
		CostNanoUSD:           nil,
	}
	comp := proxyCompletion(rec)
	if comp.Cost == nil || *comp.Cost <= 0 {
		t.Fatalf("expected fallback Claude cost, got %v", comp.Cost)
	}
}

func TestClaudeEmptyTitleNotFrozenWhenTranscriptArrivesLater(t *testing.T) {
	ledger := openTestUsageLedger(t)
	directory := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", directory)

	// First read before transcript exists -> empty title.
	first, err := ledger.claudeCodeTaskTitles(map[string]bool{"late-session": true})
	if err != nil {
		t.Fatal(err)
	}
	if first["late-session"] != "" {
		t.Fatalf("expected empty title initially, got %q", first["late-session"])
	}

	// Now transcript appears.
	project := filepath.Join(directory, "projects", "p1")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "late-session.jsonl"), []byte(`{"type":"user","message":{"content":"Arrived later"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := ledger.claudeCodeTaskTitles(map[string]bool{"late-session": true})
	if err != nil {
		t.Fatal(err)
	}
	if second["late-session"] != "Arrived later" {
		t.Fatalf("expected title after transcript arrived, got %q", second["late-session"])
	}
}
