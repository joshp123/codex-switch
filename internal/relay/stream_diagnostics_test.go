package relay

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
)

func TestStreamCloseReportsOnlyUnfinishedModelWork(t *testing.T) {
	var diagnostics bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&diagnostics)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	observer := newTerminalResponseObserver("task-1", "test-client", "account-2", nil, &recordedHTTPCompletion{})
	observer.accountDisplayName = "Codex 2"
	observer.modelSlug = "astra"
	observer.logReadError(io.EOF)
	if diagnostics.Len() != 0 {
		t.Fatal("idle connection close was reported as interrupted work")
	}
	observer.observeStructuredEvent([]byte(`{"type":"response.created"}`))
	observer.logReadError(io.EOF)
	for _, context := range []string{"OpenAI closed the stream before the astra response completed", "task task-1 from test-client on Codex 2"} {
		if !strings.Contains(diagnostics.String(), context) {
			t.Fatalf("interruption has no %s: %s", context, diagnostics.String())
		}
	}
	diagnostics.Reset()
	observer.observeStructuredEvent([]byte(`{"type":"response.completed","response":{"id":"reply-1","model":"astra","usage":{"input_tokens":10,"output_tokens":2}}}`))
	observer.logReadError(io.EOF)
	if diagnostics.Len() != 0 {
		t.Fatal("completed connection close was reported as interrupted work")
	}
	observer.observeStructuredEvent([]byte(`{"type":"response.failed","response":{"id":"reply-2","error":{"type":"server_error","message":"provider message"}}}`))
	if !strings.Contains(diagnostics.String(), "OpenAI failed the astra response: server_error provider message") {
		t.Fatalf("provider failure is logged without OpenAI's reason: %s", diagnostics.String())
	}
	diagnostics.Reset()
	observer.observeStructuredEvent([]byte(`{"type":"response.incomplete","response":{"id":"reply-3","incomplete_details":{"reason":"max_output_tokens"}}}`))
	if !strings.Contains(diagnostics.String(), "incomplete max_output_tokens") {
		t.Fatalf("incomplete response is logged without OpenAI's reason: %s", diagnostics.String())
	}
}
