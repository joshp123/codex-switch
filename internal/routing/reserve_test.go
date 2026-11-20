package routing

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

func TestSoftReservePreservesPinsAndFallsBackWhileHardReserveMovesPins(t *testing.T) {
	for _, provider := range []accounts.Provider{accounts.ProviderCodex, accounts.ProviderClaude} {
		t.Run(string(provider), func(t *testing.T) {
			database, err := store.OpenStoreDatabase(filepath.Join(t.TempDir(), "pool.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.CloseStoreDatabase()
			rememberTestAccounts(t, database, "account-1", "account-2")
			now := time.Now()
			facts := capacityWaitFacts{}
			for index, name := range []store.AccountKey{"account-1", "account-2"} {
				facts = append(facts, accounts.AccountRoutingFact{
					Provider: provider, AccountKey: name, AccountState: accounts.AccountReady,
					Credentials: accounts.Credentials{AccountID: string(name), AccessToken: "test"}, SupportedModelSlugs: []string{"test-model"},
					AllowanceSample: accounts.AllowanceSample{Allowed: true, ObservedAt: now, Windows: []accounts.ReportedWindow{
						{DurationSeconds: 7 * 86400, UsedPercent: float64(20 + index*20), ResetsAt: now.Add(time.Hour)},
					}},
				})
			}
			router := CreateTaskRouter(database, facts)
			route := func(task, parent store.TaskID, want store.AccountKey) {
				t.Helper()
				decision, err := router.RouteForProviderTask(task, store.ModelTurnWorkKind, "test-model", provider, parent)
				if err != nil || decision.AccountKey != want {
					t.Fatalf("route %s: account=%s error=%v; want %s", task, decision.AccountKey, err, want)
				}
			}
			route("pinned", "", "account-1")
			facts[0].Reserve = accounts.AccountReserve{SoftPercent: 80, HardPercent: 10}
			route("pinned", "", "account-1")
			route("new", "", "account-2")
			route("fork", "pinned", "account-2")
			facts[1].AccountState = accounts.AccountCredentialsUnavailable
			route("fallback", "", "account-1")
			facts[1].AccountState = accounts.AccountReady
			facts[0].Reserve.HardPercent = 80
			route("pinned", "", "account-2")
		})
	}
}

func TestModelSubLimitDoesNotTriggerClaudeReserve(t *testing.T) {
	database, err := store.OpenStoreDatabase(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.CloseStoreDatabase()
	rememberTestAccounts(t, database, "claude-1")
	now := time.Now()
	facts := capacityWaitFacts{{
		Provider: accounts.ProviderClaude, AccountKey: "claude-1", AccountState: accounts.AccountReady,
		Credentials: accounts.Credentials{AccountID: "claude-1", AccessToken: "test"},
		Reserve:     accounts.AccountReserve{SoftPercent: 15, HardPercent: 5},
		AllowanceSample: accounts.AllowanceSample{Allowed: true, ObservedAt: now, Windows: []accounts.ReportedWindow{
			{DurationSeconds: 7 * 86400, UsedPercent: 62, ResetsAt: now.Add(24 * time.Hour)},
			{DurationSeconds: 7 * 86400, ModelName: "Fable", UsedPercent: 96, ResetsAt: now.Add(24 * time.Hour)},
		}},
	}}
	router := CreateTaskRouter(database, facts)

	for _, model := range []string{"claude-fable-5-2", "claude-opus-5-5"} {
		decision, err := router.RouteForProviderTask(store.TaskID("task-"+model), store.ModelTurnWorkKind, model, accounts.ProviderClaude, "")
		if err != nil || decision.AccountKey != "claude-1" {
			t.Fatalf("route %s with Fable at 96%%: account=%s err=%v; want claude-1", model, decision.AccountKey, err)
		}
	}

	facts[0].AllowanceSample.Windows[1].UsedPercent = 100
	if _, err := router.RouteForProviderTask("task-fable-full", store.ModelTurnWorkKind, "claude-fable-5-2", accounts.ProviderClaude, ""); err == nil {
		t.Fatalf("expected Fable at 100%% to block Fable")
	}
	decision, err := router.RouteForProviderTask("task-opus-after-fable-full", store.ModelTurnWorkKind, "claude-opus-5-5", accounts.ProviderClaude, "")
	if err != nil || decision.AccountKey != "claude-1" {
		t.Fatalf("route Opus with Fable at 100%%: account=%s err=%v; want claude-1", decision.AccountKey, err)
	}
}
