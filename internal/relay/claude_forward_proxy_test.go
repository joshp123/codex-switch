package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
)

const forwardProxyCapability = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const claudeForwardResponse = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":\"claude-haiku-5-5\",\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":8,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

type forwardProxyRouter struct {
	TaskRouter
	mutex sync.Mutex
	tasks []store.TaskID
}

func (router *forwardProxyRouter) RouteForProviderTask(task store.TaskID, kind store.WorkKind, model string, provider accounts.Provider, parent store.TaskID) (routing.RouteDecision, error) {
	if kind != store.ModelTurnWorkKind || provider != accounts.ProviderClaude || model != "claude-haiku-5-5" {
		return routing.RouteDecision{}, fmt.Errorf("unexpected pooled request: %s %s %s", kind, provider, model)
	}
	router.mutex.Lock()
	router.tasks = append(router.tasks, task)
	router.mutex.Unlock()
	return routing.RouteDecision{AccountName: "account-2", Credentials: accounts.Credentials{AccessToken: "pool-token"}}, nil
}

type forwardProxyLedger struct {
	mutex   sync.Mutex
	records []store.CompletedResponseRecord
}

func (ledger *forwardProxyLedger) RecordCompletedResponse(record store.CompletedResponseRecord) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	ledger.records = append(ledger.records, record)
	return nil
}

func startTestClaudeForwardProxy(t *testing.T, upstream *httptest.Server) (*ClaudeForwardProxy, *httptest.Server, *http.Client, *forwardProxyRouter, *forwardProxyLedger, string) {
	t.Helper()
	origin, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	router, ledger := &forwardProxyRouter{}, &forwardProxyLedger{}
	traffic, err := CreateTrafficRelayHandler(RelayHandlerOptions{
		UpstreamOrigin: origin, ClaudeUpstreamOrigin: origin, ClientCapability: forwardProxyCapability,
		TaskRouter: router, CompletionLedger: ledger,
	})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	proxy, err := CreateClaudeForwardProxy(directory, forwardProxyCapability, traffic, traffic)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	ca, err := os.ReadFile(ClaudeProxyCAPath(directory))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("no public CA certificate")
	}
	proxyURL, _ := url.Parse(ClaudeProxyURL(strings.TrimPrefix(server.URL, "http://"), forwardProxyCapability))
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	return proxy, server, &http.Client{Transport: transport, Timeout: 5 * time.Second}, router, ledger, directory
}

func TestClaudeForwardProxyPoolsOnlyModelPostsAndKeepsOneRelay(t *testing.T) {
	body := "{\n  \"model\": \"claude-haiku-5-5\", \"stream\": true, \"messages\": []\n}\n"
	var mutex sync.Mutex
	var received []string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, _ := io.ReadAll(request.Body)
		mutex.Lock()
		received = append(received, request.Method+" "+request.URL.RequestURI()+" "+request.Header.Get("Authorization"))
		mutex.Unlock()
		if request.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials leaked upstream")
		}
		if request.Method == http.MethodPost && request.URL.Path == "/v1/messages" {
			if request.Header.Get("Accept-Encoding") != "identity" {
				t.Error("Claude stream did not request the measured uncompressed response path")
			}
			if string(payload) != body || request.Header.Get("Anthropic-Version") != "2023-06-01" || request.Header.Get("X-Api-Key") != "" {
				t.Error("pooled request bytes or headers differ from the existing relay contract")
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(writer, claudeForwardResponse)
			return
		}
		writer.Header().Set("X-Account-Feature", "unchanged")
		writer.WriteHeader(201)
		writer.Write(payload)
	}))
	defer upstream.Close()
	_, server, client, router, ledger, directory := startTestClaudeForwardProxy(t, upstream)
	for _, target := range []string{
		"https://api.anthropic.com/v1/messages?beta=true",
		server.URL + "/client/" + forwardProxyCapability + "/anthropic/v1/messages?beta=true",
	} {
		request, _ := http.NewRequest("POST", target, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer signed-in-user")
		request.Header.Set("X-Api-Key", "signed-in-key")
		request.Header.Set("X-Claude-Code-Session-Id", "session-test")
		requestClient := client
		if strings.HasPrefix(target, server.URL) {
			requestClient = server.Client()
		}
		response, err := requestClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || string(payload) != claudeForwardResponse {
			t.Fatalf("stream changed: status %d error %v body %q", response.StatusCode, err, payload)
		}
	}
	for _, endpoint := range []struct{ method, path string }{
		{"GET", "/v1/messages"}, {"POST", "/v1/messages/count_tokens?beta=true"}, {"POST", "/v1/settings"},
	} {
		request, _ := http.NewRequest(endpoint.method, "https://api.anthropic.com"+endpoint.path, strings.NewReader("account feature body"))
		request.Header.Set("Authorization", "Bearer signed-in-user")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 201 || string(payload) != "account feature body" || response.Header.Get("X-Account-Feature") != "unchanged" {
			t.Fatal("account feature response changed")
		}
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(received) != 5 || received[0] != "POST /v1/messages?beta=true Bearer pool-token" || received[1] != received[0] {
		t.Fatalf("model work replayed or entered different paths: %q", received)
	}
	for _, request := range received[2:] {
		if !strings.HasSuffix(request, "Bearer signed-in-user") {
			t.Fatalf("account feature used pooled credentials: %s", request)
		}
	}
	router.mutex.Lock()
	defer router.mutex.Unlock()
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	if len(router.tasks) != 2 || router.tasks[0] != "claude:code:session-test" || router.tasks[1] != router.tasks[0] || len(ledger.records) != 2 {
		t.Fatal("entry points do not share task identity and accounting")
	}
	for _, record := range ledger.records {
		if record.TaskID != router.tasks[0] || record.AccountName != "account-2" || record.InputTokens != 20 || record.CachedInputTokens != 8 || record.OutputTokens != 3 {
			t.Fatalf("incorrect usage record: %+v", record)
		}
	}
	ca, _ := os.ReadFile(ClaudeProxyCAPath(directory))
	if _, err := CreateClaudeProxyCertificate(directory); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(ClaudeProxyCAPath(directory))
	private, err := os.Stat(filepath.Join(directory, "claude-proxy-authority.pem"))
	if err != nil || private.Mode().Perm() != 0o600 || !bytes.Equal(ca, again) || bytes.Contains(ca, []byte("PRIVATE KEY")) {
		t.Fatal("authority changed, exposed its key or has unsafe permissions")
	}
}

func TestClaudeForwardProxyTunnelsOtherHostsWithoutDecrypting(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "opaque TLS response")
	}))
	defer upstream.Close()
	_, server, _, _, _, _ := startTestClaudeForwardProxy(t, upstream)
	proxyURL, _ := url.Parse(ClaudeProxyURL(strings.TrimPrefix(server.URL, "http://"), forwardProxyCapability))
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "opaque TLS response" || !bytes.Equal(response.TLS.PeerCertificates[0].Raw, upstream.Certificate().Raw) {
		t.Fatal("other host's TLS was altered")
	}
	unauthenticated, _ := http.NewRequest("CONNECT", server.URL, nil)
	unauthenticated.Host = "api.anthropic.com:443"
	rejected, err := server.Client().Do(unauthenticated)
	if err != nil {
		t.Fatal(err)
	}
	rejected.Body.Close()
	if rejected.StatusCode != 407 {
		t.Fatal("forward proxy accepted a client without its private capability")
	}
}

func TestClaudeForwardProxyDrainLetsStreamsFinishAndClosesIdleTunnels(t *testing.T) {
	finishOther := make(chan struct{})
	finishModel := make(chan struct{})
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finishModel) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(writer, ": stream admitted\n\n")
		writer.(http.Flusher).Flush()
		if request.Method == "POST" && request.URL.Path == "/v1/messages" {
			<-finishModel
			io.WriteString(writer, claudeForwardResponse)
		} else {
			<-finishOther
			io.WriteString(writer, ": finished\n\n")
		}
	}))
	defer upstream.Close()
	defer finish()
	proxy, server, client, _, ledger, _ := startTestClaudeForwardProxy(t, upstream)
	request, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-haiku-5-5","stream":true}`))
	modelResponse, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer modelResponse.Body.Close()
	otherResponse, err := client.Get("https://api.anthropic.com/v1/code/sessions/session-test/worker/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer otherResponse.Body.Close()
	otherClosed := make(chan error, 1)
	go func() { _, err := io.ReadAll(otherResponse.Body); otherClosed <- err }()

	peer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	go func() {
		connection, err := peer.Accept()
		if err == nil {
			defer connection.Close()
			_, _ = io.Copy(connection, connection)
		}
	}()
	tunnel, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	tunnel.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(tunnel, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", peer.Addr(), peer.Addr(), base64.StdEncoding.EncodeToString([]byte("codex-switch:"+forwardProxyCapability)))
	reader := bufio.NewReader(tunnel)
	connected, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil || connected.StatusCode != 200 {
		t.Fatalf("CONNECT failed: %v", err)
	}
	tunnelClosed := make(chan error, 1)
	go func() { _, err := io.ReadAll(reader); tunnelClosed <- err }()

	drainStarted := time.Now()
	proxy.drainConnectionsWithin(100 * time.Millisecond)
	if err := server.Config.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	proxy.relay.BeginRelayDrain()
	drained := make(chan error, 1)
	go func() { drained <- proxy.relay.WaitForRelayDrain(context.Background()) }()
	for _, closed := range []<-chan error{tunnelClosed} {
		select {
		case err := <-closed:
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatalf("non-model connection timed out instead of closing: %v", err)
			}
			if time.Since(drainStarted) < 100*time.Millisecond {
				t.Fatal("non-model connection was cut before its grace expired")
			}
		case <-time.After(time.Second):
			t.Fatal("non-model connection survived its grace")
		}
	}
	select {
	case <-otherClosed:
		t.Fatal("drain cut the active non-model stream")
	default:
	}
	close(finishOther)
	<-otherClosed
	select {
	case <-drained:
		t.Fatal("drain cut the model response after the grace period")
	default:
	}
	finish()
	body, err := io.ReadAll(modelResponse.Body)
	if err != nil || string(body) != ": stream admitted\n\n"+claudeForwardResponse {
		t.Fatalf("model stream did not finish intact past the grace: %v %q", err, body)
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed model work did not drain")
	}
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	if len(ledger.records) != 1 {
		t.Fatal("model response lost its completion record")
	}
}
