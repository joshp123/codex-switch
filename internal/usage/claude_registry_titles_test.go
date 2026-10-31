package usage

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeClaudeRegistryFixture(t testing.TB, path, session, name string, revision int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
		UpdatedAt int64  `json:"updatedAt"`
		PID       int    `json:"pid"`
		Status    string `json:"status"`
	}{session, name, revision, os.Getpid(), "idle"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	modified := time.Unix(1_700_000_000+revision, 0)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeRegistryRenameAndEndedSessionTitle(t *testing.T) {
	ledger := openTestUsageLedger(t)
	directory := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", directory)
	project := filepath.Join(directory, "projects", "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "session.jsonl")
	writeTranscript := func(name string) {
		t.Helper()
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString(`{"type":"custom-title","customTitle":"` + name + `"}` + "\n")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("append transcript: %v %v", writeErr, closeErr)
		}
	}
	check := func(expected string) {
		t.Helper()
		var actual string
		err := ledger.visitClaudeTaskMetadata(maps.Keys(map[string]bool{"claude:code:session": true}), func(item taskMetadata) { actual = item.Title })
		if err != nil {
			t.Fatal(err)
		}
		if actual != expected {
			t.Fatalf("title = %q, want %q", actual, expected)
		}
	}
	registry := filepath.Join(directory, "sessions", "123.json")
	writeTranscript("Transcript title")
	writeClaudeRegistryFixture(t, registry, "session", "App title", 1)
	check("App title")
	if len(ledger.sessionFiles.Titles) != 0 {
		t.Fatal("a named running session read its transcript")
	}
	writeClaudeRegistryFixture(t, registry, "session", "New title", 2)
	check("New title")
	if err := os.WriteFile(registry, []byte(`{"sessionId":"session","name":`), 0600); err != nil {
		t.Fatal(err)
	}
	check("New title")
	writeClaudeRegistryFixture(t, registry, "session", "", 3)
	check("Transcript title")
	writeTranscript("Updated transcript title")
	check("Transcript title")
	writeClaudeRegistryFixture(t, registry, "session", "", 4)
	check("Updated transcript title")
	writeClaudeRegistryFixture(t, registry, "session", "Final app title", 5)
	check("Final app title")
	if err := os.Rename(registry, filepath.Join(directory, "ended.json")); err != nil {
		t.Fatal(err)
	}
	writeTranscript("Changed after session ended")
	check("Final app title")
	restarted, err := OpenUsageLedger(ledger.database, ledger.dataDirectory, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger = restarted
	check("Final app title")
}

func TestClaudeEndedSessionReadsTranscriptOnce(t *testing.T) {
	ledger := openTestUsageLedger(t)
	directory := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", directory)
	project := filepath.Join(directory, "projects", "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"content":"First prompt"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := ledger.claudeCodeTaskTitles(map[string]bool{"session": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(directory, "moved.jsonl")); err != nil {
		t.Fatal(err)
	}
	second, err := ledger.claudeCodeTaskTitles(map[string]bool{"session": true})
	if err != nil {
		t.Fatal(err)
	}
	if first["session"] != "First prompt" || second["session"] != first["session"] {
		t.Fatal("an ended session lost its previously read transcript title")
	}
}
