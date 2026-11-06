package store

import (
	"fmt"
	"testing"
)

// rememberTestAccounts records a folder for each key, as signing in does.
func rememberTestAccounts(t *testing.T, database *StoreDatabase, keys ...AccountKey) {
	t.Helper()
	for index, key := range keys {
		if err := database.RememberAccountIdentity(AccountIdentity{
			AccountFolder: fmt.Sprintf("account-%d", index+1), AccountKey: key,
			ProviderAccountID: "provider-" + string(key), Email: string(key) + "@example.com",
		}); err != nil {
			t.Fatal(err)
		}
	}
}
