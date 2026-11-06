package usage

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

func TestTwoGenerationsImportSameSessionWithoutReplacingRows(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "session.jsonl")
	header := `{"type":"session","version":3,"id":"shared","timestamp":"2026-10-22T10:00:00Z"}` + "\n"
	message := `{"type":"message","id":"entry","timestamp":"2026-10-22T10:00:01Z","message":{"role":"assistant","provider":"openai-codex","model":"gpt-5.6-luna","responseId":"response","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":0,"reasoning":0,"totalTokens":12}}}` + "\n"
	if err := os.WriteFile(path, []byte(header+message), 0600); err != nil {
		t.Fatal(err)
	}
	first := openSharedImportLedger(t, filepath.Join(directory, "usage.db"))
	second := openSharedImportLedger(t, filepath.Join(directory, "usage.db"))
	start := make(chan struct{})
	var wait sync.WaitGroup
	failures := make(chan error, 2)
	for _, ledger := range []*UsageLedger{first, second} {
		wait.Add(1)
		go func(ledger *UsageLedger) {
			defer wait.Done()
			<-start
			err := ledger.importNativeSessions(context.Background(), t.TempDir(), directory, time.Time{})
			failures <- err
		}(ledger)
	}
	close(start)
	wait.Wait()
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	before, err := readUsageRowWatermarks(context.Background(), first.sqlDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if before.NativeObservations != 1 || before.NativeSources <= 0 || before.MutationGeneration != 0 {
		t.Fatalf("duplicate import rewrote rows: %+v", before)
	}
	// A later cursor update remains visible above the source watermark while the
	// original response keeps its rowid and does not trigger a reset.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(`{"type":"session_info","name":"Renamed"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	// A title-only append has no completed response; the hourly audit finds it.
	second.piDiscovery.LastWalk = time.Now().Add(-time.Hour)
	if err = second.importNativeSessions(context.Background(), t.TempDir(), directory, time.Time{}); err != nil {
		t.Fatal(err)
	}
	after, err := readUsageRowWatermarks(context.Background(), first.sqlDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if after.NativeSources <= before.NativeSources || after.NativeObservations != before.NativeObservations || after.MutationGeneration != before.MutationGeneration {
		t.Fatalf("append marks before=%+v after=%+v", before, after)
	}
	// The old binary replaces a source cursor. SQLite assigns its new row
	// a rowid above the watermark held by the other generation.
	if _, err = second.sqlDatabase.Exec(`INSERT OR REPLACE INTO native_usage_sources SELECT * FROM native_usage_sources`); err != nil {
		t.Fatal(err)
	}
	oldWriter, err := readUsageRowWatermarks(context.Background(), first.sqlDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if oldWriter.NativeSources <= after.NativeSources || oldWriter.MutationGeneration != after.MutationGeneration {
		t.Fatalf("old writer source update invisible: before=%+v after=%+v", after, oldWriter)
	}
	// A rewrite resets the raw rows. Its DELETE is visible to the other connection
	// even if SQLite reuses the old highest observation rowid.
	replacement := header + `{"type":"session_info","name":"Reset"}` + "\n"
	if err = os.WriteFile(path, []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	if err = second.importNativeSessions(context.Background(), t.TempDir(), directory, time.Time{}); err != nil {
		t.Fatal(err)
	}
	reset, err := readUsageRowWatermarks(context.Background(), first.sqlDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if reset.NativeObservations != 0 || reset.MutationGeneration <= after.MutationGeneration {
		t.Fatalf("reset not observable: before=%+v after=%+v", after, reset)
	}
}

func openSharedImportLedger(t *testing.T, path string) *UsageLedger {
	t.Helper()
	database, err := store.OpenStoreDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.CloseStoreDatabase() })
	rememberTestAccount(t, database)
	ledger, err := OpenUsageLedger(database, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}
