package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
)

func TestHTTPUsageLimitRejectionAllowsClientRetryWithoutProxyReplay(t *testing.T) {
	for _, websocketHandshake := range []bool{false, true} {
		name := "HTTP response request"
		if websocketHandshake {
			name = "WebSocket handshake"
		}
		t.Run(name, func(t *testing.T) {
			body := `{"error":{"type":"usage_limit_reached","resets_at":2000000000}}`
			response, router, requests := serveHTTPRejection(t, body, "application/json", websocketHandshake)
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", response.Code)
			}
			if strings.Contains(response.Body.String(), "usage_limit_reached") {
				t.Fatal("terminal usage-limit body reached client")
			}
			if !strings.Contains(response.Body.String(), "service unavailable") {
				t.Fatal("Pi requires a recognizable transient error after its provider discards the HTTP status")
			}
			if router.reportCount != 1 || router.task != "task-1" || router.account != "account-1" || !router.resetsAt.Equal(time.Unix(2000000000, 0)) {
				t.Fatalf("incorrect exhaustion report: %+v", router)
			}
			if requests != 1 {
				t.Fatalf("proxy sent %d requests, want exactly one", requests)
			}
		})
	}
}

func TestHTTPUnrelated429PreservesStatusAndBody(t *testing.T) {
	for _, test := range []struct{ name, contentType, body string }{
		{"other provider error", "application/json", "{\n  \"error\": {\"type\": \"rate_limit_exceeded\", \"message\": \"slow down\"}\n}\n"},
		{"plain text rejection", "text/plain", "Too many requests\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, router, requests := serveHTTPRejection(t, test.body, test.contentType, false)
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", response.Code)
			}
			if response.Body.String() != test.body {
				t.Fatalf("body changed: got %q, want %q", response.Body.String(), test.body)
			}
			if response.Header().Get("Content-Type") != test.contentType || response.Header().Get("Retry-After") != "17" {
				t.Fatal("rejection headers changed")
			}
			if router.reportCount != 0 {
				t.Fatal("unrelated rejection reported as account exhaustion")
			}
			if requests != 1 {
				t.Fatalf("proxy sent %d requests, want exactly one", requests)
			}
		})
	}
}

func serveHTTPRejection(t *testing.T, body, contentType string, websocketHandshake bool) (*httptest.ResponseRecorder, *httpRejectionRouter, int32) {
	t.Helper()
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", contentType)
		response.Header().Set("Retry-After", "17")
		response.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(response, body)
	}))
	t.Cleanup(upstream.Close)
	origin, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	router := &httpRejectionRouter{}
	capability := strings.Repeat("a", 43)
	handler, err := CreateTrafficRelayHandler(RelayHandlerOptions{
		UpstreamOrigin: origin, ClientCapability: capability,
		TaskRouter: router, CompletionLedger: &unusedHTTPRejectionLedger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	method := http.MethodPost
	if websocketHandshake {
		method = http.MethodGet
	}
	request := httptest.NewRequest(method, "/client/"+capability+"/backend-api/codex/responses", strings.NewReader(`{"model":"example"}`))
	request.Header.Set("Thread-ID", "task-1")
	if websocketHandshake {
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("Sec-WebSocket-Version", "13")
		request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response, router, requests.Load()
}

type httpRejectionRouter struct {
	TaskRouter
	reportCount int
	task        store.TaskID
	account     store.AccountKey
	resetsAt    time.Time
}

func (*httpRejectionRouter) RouteForTask(store.TaskID, store.WorkKind, string) (routing.RouteDecision, error) {
	return routing.RouteDecision{AccountKey: "account-1", Credentials: accounts.Credentials{AccessToken: "test-token", AccountID: "test-account"}}, nil
}

func (router *httpRejectionRouter) ReportExhaustedTurn(task store.TaskID, account store.AccountKey, resetsAt time.Time) string {
	router.reportCount++
	router.task, router.account, router.resetsAt = task, account, resetsAt
	return "moved"
}

type unusedHTTPRejectionLedger struct{ CompletionLedger }

func (r *httpRejectionRouter) RouteForProviderTask(id store.TaskID, kind store.WorkKind, model string, _ accounts.Provider, _ store.TaskID) (routing.RouteDecision, error) {
	return r.RouteForTask(id, kind, model)
}
