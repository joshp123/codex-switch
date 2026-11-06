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
