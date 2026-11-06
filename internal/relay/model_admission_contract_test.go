package relay

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
	"github.com/klauspost/compress/zstd"
)

type modelAdmissionRouter struct {
	TaskRouter
	account            store.AccountKey
	model              string
	unsupported        int
	declineUnsupported bool
}

func (r *modelAdmissionRouter) RouteForTask(_ store.TaskID, _ store.WorkKind, model string) (routing.RouteDecision, error) {
	r.model = model
	return routing.RouteDecision{AccountKey: r.account, Credentials: accounts.Credentials{AccessToken: "test", AccountID: "test"}}, nil
}
func (r *modelAdmissionRouter) ReportUnsupportedModel(_ store.TaskID, _ store.AccountKey, _ string) bool {
	r.unsupported++
	return !r.declineUnsupported
}
func (r *modelAdmissionRouter) ReportExhaustedTurn(_ store.TaskID, _ store.AccountKey, _ time.Time) string {
	return ""
}

type partialFrameSink struct{ bytes.Buffer }

func (s *partialFrameSink) Write(b []byte) (int, error) { return s.Buffer.Write(b[:min(3, len(b))]) }
func (s *partialFrameSink) Close() error                { return nil }

func maskedAdmissionFrame(opcode byte, payload []byte) []byte {
	frame := reconnectTestFrame(opcode, payload)
	headerLength := len(frame) - len(payload)
	frame[1] |= 0x80
	result := append([]byte(nil), frame[:headerLength]...)
	mask := []byte{3, 1, 4, 1}
	result = append(result, mask...)
	for i, b := range payload {
		result = append(result, b^mask[i%4])
	}
	return result
}

func TestModelGatePreservesCompressedMaskedMessagesAcrossPartialWrites(t *testing.T) {
	sink := &partialFrameSink{}
	connection := createObservedWebSocketConnection(sink, "permessage-deflate", nil)
	var observed []string
	connection.observeClientMessages(func(message []byte) bool { observed = append(observed, string(message)); return false })
	var compressed bytes.Buffer
	compressor, _ := flate.NewWriter(&compressed, flate.DefaultCompression)
	defer compressor.Close()
	var frames []byte
	messages := []string{`{"type":"response.create","model":"gpt-5.6-luna"}`, `{"type":"response.create","model":"gpt-6-astra"}`}
	for _, message := range messages {
		compressed.Reset()
		compressor.Write([]byte(message))
		compressor.Flush()
		payload := compressed.Bytes()
		payload = payload[:len(payload)-4]
		frames = append(frames, maskedAdmissionFrame(0xc1, payload)...)
	}
	for _, b := range frames {
		if n, err := connection.Write([]byte{b}); err != nil || n != 1 {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if !bytes.Equal(frames, sink.Bytes()) {
		t.Fatal("outgoing wire bytes changed")
	}
	if len(observed) != 2 || observed[0] != messages[0] || observed[1] != messages[1] {
		t.Fatalf("observed %q", observed)
	}
}

func TestModelGateWithholdsFragmentedRejectedMessageButForwardsPing(t *testing.T) {
	sink := &partialFrameSink{}
	connection := createObservedWebSocketConnection(sink, "", nil)
	connection.observeClientMessages(func(message []byte) bool {
		return string(message) == `{"type":"response.create","model":"gpt-6-astra"}`
	})
	first := maskedAdmissionFrame(0x01, []byte(`{"type":"response.create",`))
	ping := maskedAdmissionFrame(0x89, []byte("ping"))
	last := maskedAdmissionFrame(0x80, []byte(`"model":"gpt-6-astra"}`))
	if _, err := connection.Write(append(first, ping...)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sink.Bytes(), ping) {
		t.Fatal("message fragment escaped admission gate")
	}
	if _, err := connection.Write(last); !errors.Is(err, errCloseWebSocketAfterTerminalLimit) {
		t.Fatalf("rejection %v", err)
	}
	if !bytes.Equal(sink.Bytes(), ping) {
		t.Fatal("rejected message reached upstream")
	}
}

func TestModelChangeDoesNotCloseAdmittedResponse(t *testing.T) {
	router := &modelAdmissionRouter{account: "account-3"}
	observer := newTerminalResponseObserver("task", "test-client", "account-3", router, &unusedHTTPRejectionLedger{})
	create := []byte(`{"type":"response.create","model":"gpt-5.6-luna"}`)
	if observer.routeClientMessage(create) {
		t.Fatal("first request rejected")
	}
	observer.observeStructuredEvent([]byte(`{"type":"response.created","response":{"id":"first"}}`))
	router.account = "account-1"
	if observer.routeClientMessage(create) {
		t.Fatal("concurrent request closed admitted work")
	}
	observer.observeStructuredEvent([]byte(`{"type":"response.created","response":{"id":"second"}}`))
	observer.observeStructuredEvent([]byte(`{"type":"response.completed"}`))
	rejection := []byte(`{"type":"error","error":{"type":"usage_limit_reached","resets_at":2000000000}}`)
	if observer.observeStructuredEvent(rejection) {
		t.Fatal("error closed second admitted response")
	}
	if observer.admittedRequests != 1 {
		t.Fatal("lost concurrent admitted request")
	}
	observer.observeStructuredEvent([]byte(`{"type":"response.completed"}`))
	if !observer.routeClientMessage(create) {
		t.Fatal("idle socket did not reconnect for new route")
	}
}

func TestSocketWithoutTaskIDDoesNotReconnectInALoop(t *testing.T) {
	router := &modelAdmissionRouter{account: "account-1"}
	observer := newTerminalResponseObserver("", "test-client", "account-3", router, &unusedHTTPRejectionLedger{})
	create := []byte(`{"type":"response.create","model":"gpt-6-astra"}`)
	if observer.routeClientMessage(create) {
		t.Fatal("a socket without a task ID reconnected; no pin would carry the new account, so it would loop")
	}
	observer.observeStructuredEvent([]byte(`{"type":"response.completed"}`))
	unsupported := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}}`)
	if observer.observeStructuredEvent(unsupported) {
		t.Fatal("an unsupported model on a socket without a task ID reconnected into the same account")
	}
}

func TestOnlyExactUnsupportedModelBeforeCreationReconnects(t *testing.T) {
	for _, started := range []bool{false, true} {
		router := &modelAdmissionRouter{}
		observer := newTerminalResponseObserver("task", "test-client", "account-3", router, &unusedHTTPRejectionLedger{})
		observer.modelSlug = "gpt-6-astra"
		if started {
			observer.activeResponses = 1
		}
		observer.admittedRequests = 1
		unrelated := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad input"}}`)
		if observer.observeStructuredEvent(unrelated) {
			t.Fatal("arbitrary error caused reconnect")
		}
		exact := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}}`)
		if observer.observeStructuredEvent(exact) == started {
			t.Fatal("wrong admission boundary")
		}
		if router.unsupported != map[bool]int{false: 1, true: 0}[started] {
			t.Fatal("wrong unsupported report")
		}
	}
}

func TestHTTPModelRoutingPreservesOriginalBody(t *testing.T) {
	for _, encoding := range []string{"", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			body := []byte("{\n  \"model\": \"gpt-6-astra\", \"input\": []\n}\n")
			if encoding == "zstd" {
				encoder, err := zstd.NewWriter(nil)
				if err != nil {
					t.Fatal(err)
				}
				body = encoder.EncodeAll(body, nil)
				encoder.Close()
			}
			var received []byte
			var receivedEncoding string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received, _ = io.ReadAll(r.Body)
				receivedEncoding = r.Header.Get("Content-Encoding")
				w.WriteHeader(204)
			}))
			defer upstream.Close()
			origin, _ := url.Parse(upstream.URL)
			router := &modelAdmissionRouter{account: "account-1"}
			capability := strings.Repeat("a", 43)
			handler, err := CreateTrafficRelayHandler(RelayHandlerOptions{UpstreamOrigin: origin, ClientCapability: capability, TaskRouter: router, CompletionLedger: &unusedHTTPRejectionLedger{}})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/client/"+capability+"/backend-api/codex/responses", bytes.NewReader(body))
			request.Header.Set("Thread-ID", "task")
			request.Header.Set("Content-Encoding", encoding)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 204 || router.model != "gpt-6-astra" || !bytes.Equal(received, body) || receivedEncoding != encoding {
				t.Fatalf("status=%d model=%q encoding=%q preserved=%t", response.Code, router.model, receivedEncoding, bytes.Equal(received, body))
			}
		})
	}
}

func TestHTTPUnsupportedModelReconnectRequiresAlternative(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"invalid_request_error","message":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}}`,
		`{"detail":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}`,
	} {
		for _, alternative := range []bool{false, true} {
			requests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				io.WriteString(w, body)
			}))
			origin, _ := url.Parse(upstream.URL)
			router := &modelAdmissionRouter{account: "account-3", declineUnsupported: !alternative}
			capability := strings.Repeat("a", 43)
			handler, err := CreateTrafficRelayHandler(RelayHandlerOptions{UpstreamOrigin: origin, ClientCapability: capability, TaskRouter: router, CompletionLedger: &unusedHTTPRejectionLedger{}})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/client/"+capability+"/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-6-astra"}`))
			request.Header.Set("Thread-ID", "task")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			upstream.Close()
			if alternative && response.Code != 502 {
				t.Fatalf("alternative status %d", response.Code)
			}
			if !alternative && (response.Code != 400 || response.Body.String() != body) {
				t.Fatal("original rejection lost without alternative")
			}
			if requests != 1 || router.unsupported != 1 {
				t.Fatalf("requests=%d reports=%d", requests, router.unsupported)
			}
		}
	}
}

func (r *modelAdmissionRouter) RouteForModelCatalogue() (routing.RouteDecision, error) {
	return routing.RouteDecision{AccountKey: "catalogue-account", Credentials: accounts.Credentials{AccessToken: "catalogue-token", AccountID: "catalogue-id"}}, nil
}

func TestModelCatalogueUsesIndependentRouteAndPreservesProviderBytes(t *testing.T) {
	body := "{\n  \"models\": [{\"slug\":\"gpt-6-astra\"}]\n}\n"
	var accountID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID = r.Header.Get("ChatGPT-Account-ID")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer upstream.Close()
	origin, _ := url.Parse(upstream.URL)
	capability := strings.Repeat("a", 43)
	router := &modelAdmissionRouter{account: "task-account"}
	handler, err := CreateTrafficRelayHandler(RelayHandlerOptions{UpstreamOrigin: origin, ClientCapability: capability, TaskRouter: router, CompletionLedger: &unusedHTTPRejectionLedger{}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/client/"+capability+"/backend-api/codex/models?client_version=0.153.1", nil)
	request.Header.Set("Thread-ID", "pinned-task")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != body || accountID != "catalogue-id" {
		t.Fatalf("status=%d account=%q body=%q", response.Code, accountID, response.Body.String())
	}
}

type recordedHTTPCompletion struct {
	records []store.CompletedResponseRecord
}

func (ledger *recordedHTTPCompletion) RecordCompletedResponse(record store.CompletedResponseRecord) error {
	ledger.records = append(ledger.records, record)
	return nil
}

func TestStreamingRequestAccountsForSSEWithMissingOrSynthesizedContentType(t *testing.T) {
	for _, contentType := range []string{"", "text/plain; charset=utf-8"} {
		t.Run(contentType, func(t *testing.T) {
			body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-http\",\"model\":\"gpt-5.6-luna\",\"service_tier\":\"default\",\"usage\":{\"input_tokens\":13631,\"input_tokens_details\":{\"cached_tokens\":9984,\"cache_write_tokens\":0},\"output_tokens\":5,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header()["Content-Type"] = nil
				if contentType != "" {
					w.Header().Set("Content-Type", contentType)
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			origin, _ := url.Parse(upstream.URL)
			ledger := &recordedHTTPCompletion{}
			capability := strings.Repeat("a", 43)
			handler, err := CreateTrafficRelayHandler(RelayHandlerOptions{UpstreamOrigin: origin, ClientCapability: capability, TaskRouter: &modelAdmissionRouter{account: "account-1"}, CompletionLedger: ledger})
			if err != nil {
				t.Fatal(err)
			}
			encoder, err := zstd.NewWriter(nil)
			if err != nil {
				t.Fatal(err)
			}
			compressed := encoder.EncodeAll([]byte(`{"model":"gpt-5.6-luna","stream":true}`), nil)
			encoder.Close()
			request := httptest.NewRequest("POST", "/client/"+capability+"/backend-api/codex/responses", bytes.NewReader(compressed))
			request.Header.Set("Content-Encoding", "zstd")
			request.Header.Set("Thread-ID", "task-http")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 || response.Body.String() != body {
				t.Fatal("original SSE response changed")
			}
			if len(ledger.records) != 1 {
				t.Fatalf("completion count = %d", len(ledger.records))
			}
			record := ledger.records[0]
			if record.TaskID != "task-http" || record.AccountKey != "account-1" || record.InputTokens != 13631 || record.CachedInputTokens != 9984 || record.CacheWriteInputTokens == nil || *record.CacheWriteInputTokens != 0 || record.OutputTokens != 5 {
				t.Fatalf("incorrect completion: %+v", record)
			}
		})
	}
}

func (r *modelAdmissionRouter) RouteForProviderTask(id store.TaskID, kind store.WorkKind, model string, _ accounts.Provider, _ store.TaskID) (routing.RouteDecision, error) {
	return r.RouteForTask(id, kind, model)
}
