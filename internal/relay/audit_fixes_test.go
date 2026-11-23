package relay

import (
	"net/http"
	"strings"
	"testing"

	"github.com/joshp123/codex-switch/internal/store"
)

type recordingCompletionLedger struct {
	records []store.CompletedResponseRecord
}

func (l *recordingCompletionLedger) RecordCompletedResponse(record store.CompletedResponseRecord) error {
	l.records = append(l.records, record)
	return nil
}

func TestOnCompletedReplyIgnoresWarmupsAndIncompleteResponses(t *testing.T) {
	ledger := &recordingCompletionLedger{}
	completions := 0
	observer := newTerminalResponseObserver("task-1", "test-client", "chatgpt:a@example.com", nil, ledger)
	observer.onCompletedReply = func() { completions++ }

	// Warm-up (generate: false) must not trigger onCompletedReply or record usage.
	observer.observeStructuredEvent([]byte(`{"type":"response.completed","response":{"id":"resp-warmup","model":"gpt-6-luna","generate":false,"usage":{"input_tokens":10,"output_tokens":0}}}`))
	if completions != 0 || len(ledger.records) != 0 {
		t.Fatalf("warm-up triggered completions=%d records=%d", completions, len(ledger.records))
	}

	// Incomplete response records tokens but must not trigger onCompletedReply.
	observer.observeStructuredEvent([]byte(`{"type":"response.incomplete","response":{"id":"resp-inc","model":"gpt-6-luna","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":100,"output_tokens":50}}}`))
	if completions != 0 || len(ledger.records) != 1 {
		t.Fatalf("incomplete triggered completions=%d records=%d", completions, len(ledger.records))
	}

	// Real completed response records tokens and triggers onCompletedReply.
	observer.observeStructuredEvent([]byte(`{"type":"response.completed","response":{"id":"resp-ok","model":"gpt-6-luna","usage":{"input_tokens":100,"output_tokens":20}}}`))
	if completions != 1 || len(ledger.records) != 2 {
		t.Fatalf("completed triggered completions=%d records=%d", completions, len(ledger.records))
	}
}

func TestDescribeRejectionTextPreservesHTMLTitleCase(t *testing.T) {
	header := http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}
	body := []byte("<html><head><TITLE>Error SecretTokenMixedCase123</TITLE></head><body>İ</body></html>")
	got := describeRejectionText(header, body)
	if !strings.Contains(got, "SecretTokenMixedCase123") {
		t.Fatalf("describeRejectionText lost original casing: %q", got)
	}
}
