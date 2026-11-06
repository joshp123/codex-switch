package relay

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/joshp123/codex-switch/internal/store"
)

func TestCompletionAccountingPreservesCacheWritesAndExcludesPrewarm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	sqlDatabase, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDatabase.Close()
	ledger, err := store.OpenStoreDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.CloseStoreDatabase()
	rememberTestAccounts(t, ledger, "account")
	observer := newTerminalResponseObserver("task", "test-client", "account", nil, ledger)
	falseValue, trueValue := false, true
	for _, test := range []struct {
		name        string
		generate    *bool
		cacheWrites *int64
		tier        string
		keep        bool
	}{
		{"prewarm", &falseValue, accountingTokenCount(0), "auto", false},
		{"explicit generation", &trueValue, accountingTokenCount(7), "default", true},
		{"known zero", nil, accountingTokenCount(0), "default", true},
		{"unknown cache writes", nil, nil, "default", true},
		{"auto zero output inference", nil, accountingTokenCount(0), "auto", true},
		{"excess cache writes", nil, accountingTokenCount(11), "default", false},
		{"negative cache writes", nil, accountingTokenCount(-1), "default", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, transport := range []string{"HTTP JSON", "WebSocket event"} {
				id := test.name + transport
				details := map[string]any{"cached_tokens": 10}
				if test.cacheWrites != nil {
					details["cache_write_tokens"] = *test.cacheWrites
				}
				response := map[string]any{"id": id, "model": "gpt-5.6-luna", "service_tier": test.tier,
					"usage": map[string]any{"input_tokens": 20, "input_tokens_details": details, "output_tokens": 0}}
				if test.generate != nil {
					response["generate"] = *test.generate
				}
				if transport == "HTTP JSON" {
					encoded, _ := json.Marshal(response)
					observer.observeHTTPJSONBody(encoded)
				} else {
					encoded, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
					observer.observeStructuredEvent(encoded)
				}
				var writes sql.NullInt64
				err := sqlDatabase.QueryRow("SELECT cache_write_input_tokens FROM completed_responses WHERE completed_response_id=?", id).Scan(&writes)
				if !test.keep {
					if err != sql.ErrNoRows {
						t.Fatalf("%s must not record this completion; error=%v", transport, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if writes.Valid != (test.cacheWrites != nil) || writes.Valid && writes.Int64 != *test.cacheWrites {
					t.Fatalf("%s cache-write value=%+v, expected=%v", transport, writes, test.cacheWrites)
				}
			}
		})
	}

}

func accountingTokenCount(value int64) *int64 { return &value }
