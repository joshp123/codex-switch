package routing

import (
	"fmt"
	"testing"

	"github.com/joshp123/codex-switch/internal/store"
)

// rememberTestAccounts records a folder for each account key, as signing in
// does; the store writes the folder beside the key.
func rememberTestAccounts(t *testing.T, database *store.StoreDatabase, keys ...store.AccountKey) {
	t.Helper()
	for index, key := range keys {
		if err := database.RememberAccountIdentity(store.AccountIdentity{
			AccountFolder: fmt.Sprintf("account-%d", index+1), AccountKey: key,
			ProviderAccountID: string(key), Email: string(key) + "@example.test",
		}); err != nil {
			t.Fatal(err)
		}
	}
}
