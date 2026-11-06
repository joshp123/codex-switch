package routing

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

func TestClaudePlacementForkAndNextRequestLimitKeepProviderAndModel(t *testing.T) {
	database, err := store.OpenStoreDatabase(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.CloseStoreDatabase()
	rememberTestAccounts(t, database, "account-1", "account-2", "account-3")
	now := time.Now()
	facts := capacityWaitFacts{}
	for index, name := range []store.AccountKey{"account-1", "account-2", "account-3"} {
		provider := accounts.ProviderClaude
		if index == 0 {
			provider = accounts.ProviderChatGPT
		}
		facts = append(facts, accounts.AccountRoutingFact{
			Provider: provider, AccountKey: name, AccountState: accounts.AccountReady,
			Credentials:     accounts.Credentials{AccountID: string(name), AccessToken: "test"},
			AllowanceSample: accounts.AllowanceSample{Windows: []accounts.ReportedWindow{{DurationSeconds: 7 * 86400, UsedPercent: float64(index * 20), ResetsAt: now.Add(time.Hour)}}},
		})
	}
	pool := []accounts.PoolAccount{{Key: "account-1"}, {Key: "account-2", Provider: accounts.ProviderClaude}, {Key: "account-3", Provider: accounts.ProviderClaude}}
	for index := range facts {
		facts[index].DisplayName, _ = accounts.AccountDisplayName(pool, facts[index].AccountKey)
	}
	router := CreateTaskRouter(database, facts)
	route := func(task, parent store.TaskID, model string, want store.AccountKey) {
		t.Helper()
		decision, err := router.RouteForProviderTask(task, store.ModelTurnWorkKind, model, accounts.ProviderClaude, parent)
		if err != nil || decision.AccountKey != want || decision.DisplayName == "" || strings.Contains(decision.Assignment, "account-") {
			t.Fatalf("route %s: account=%s, error=%v; want %s", task, decision.AccountKey, err, want)
		}
	}
	route("parent", "", "claude-opus-5-5", "account-2")
	facts[2].AllowanceSample.Windows[0].UsedPercent = 0
	route("parent", "", "claude-opus-5-5", "account-2")
	route("fork", "parent", "claude-opus-5-5", "account-2")
	facts[1].AllowanceSample.Windows = append(facts[1].AllowanceSample.Windows, accounts.ReportedWindow{DurationSeconds: 7 * 86400, ModelID: "claude-opus-5-5", UsedPercent: 100, ResetsAt: now.Add(time.Hour)})
	route("fork", "", "claude-opus-5-5", "account-3")
	route("parent", "", "claude-sonnet-5-5", "account-2")
	facts[2].AllowanceSample.Windows[0].UsedPercent = 100
	facts[2].AccountState = accounts.AccountLimited
	if _, err := router.RouteForProviderTask("waiting", store.ModelTurnWorkKind, "claude-opus-5-5", accounts.ProviderClaude, ""); !errors.Is(err, ErrNoAvailableAccount) {
		t.Fatalf("limited Claude pool admitted work: %v", err)
	}
	metadata, err := router.RouteForProviderTask("parent", store.TaskMetadataWorkKind, "", accounts.ProviderClaude, "")
	if err != nil || metadata.AccountKey != "account-2" {
		t.Fatalf("metadata: %v, %v", metadata.AccountKey, err)
	}
	facts[2].AllowanceSample.Windows[0].ResetsAt = now.Add(-time.Second)
	route("waiting", "", "claude-opus-5-5", "account-3")
}
