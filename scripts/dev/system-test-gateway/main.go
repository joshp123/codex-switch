// system-test-gateway stands between the proxy under test and real OpenAI.
// It forwards real traffic, logs a fingerprint of every model request it
// forwards, and simulates the account conditions a system test cannot wait
// for: a weekly allowance level, an exhausted account, refused WebSockets and
// a response held mid-stream. It is never part of a live proxy.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	responsesPath = "/backend-api/codex/responses"
	usagePath     = "/backend-api/wham/usage"
)

func main() {
	dataDirectory := flag.String("data-dir", "", "proxy data directory containing account-N/auth.json")
	controlPath := flag.String("control", "", "simulation file; an empty file means real OpenAI behaviour")
	flag.Parse()
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if *dataDirectory == "" || *controlPath == "" {
		log.Fatal("data-dir and control are required")
	}
	aliases, err := loadAccountAliases(*dataDirectory)
	if err != nil {
		log.Fatal(err)
	}
	gateway := &gateway{controlPath: *controlPath, aliases: aliases, upstream: &url.URL{Scheme: "https", Host: "chatgpt.com"}}
	if _, err := gateway.readControls(); err != nil {
		log.Fatal(err)
	}
	gateway.transport = http.DefaultTransport.(*http.Transport).Clone()
	gateway.transport.DisableCompression = true
	gateway.reverseProxy = &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(gateway.upstream)
			if request.In.URL.Path == usagePath {
				request.Out.Header.Del("Accept-Encoding") // so a weekly level can be rewritten
			}
		},
		Transport:      gateway.transport,
		FlushInterval:  -1,
		ErrorLog:       log.New(io.Discard, "", 0),
		ModifyResponse: gateway.modifyResponse,
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal("cannot listen on loopback")
	}
	fmt.Printf("http://%s\n", listener.Addr())
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0), Handler: gateway}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("gateway stopped unexpectedly")
	}
}

type gateway struct {
	controlPath  string
	aliases      map[string]string // ChatGPT account ID to account-N
	upstream     *url.URL
	transport    *http.Transport
	reverseProxy *httputil.ReverseProxy
	controlMu    sync.Mutex
}

// controls is one reading of the simulation file:
//
//	weekly-used account-N PERCENT   usage reads report PERCENT of the weekly window used
//	limited account-N               the account's allowance has run out
//	exhaust-after account-N COUNT   each WebSocket gets COUNT model requests, then the allowance runs out
//	http-only                       WebSocket upgrades are refused, so clients use HTTP
//	hold-responses CLIENT           model responses for CLIENT (its Originator header) pause once started
//	break-responses                 model requests receive a malformed HTTP response
//	provider-error                  model requests receive a provider HTTP error
//	usage-error                     usage reads receive a provider HTTP error
type controls struct {
	weeklyUsed     map[string]float64
	limited        map[string]bool
	httpOnly       bool
	holdResponses  map[string]bool
	breakResponses bool
	providerError  bool
	usageError     bool
}

func (gateway *gateway) readControls() (controls, error) {
	gateway.controlMu.Lock()
	defer gateway.controlMu.Unlock()
	data, err := os.ReadFile(gateway.controlPath)
	if err != nil {
		return controls{}, errors.New("cannot read simulation control file")
	}
	parsed := controls{weeklyUsed: map[string]float64{}, limited: map[string]bool{}, holdResponses: map[string]bool{}}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 0:
		case len(fields) == 1 && fields[0] == "http-only":
			parsed.httpOnly = true
		case len(fields) == 1 && fields[0] == "break-responses":
			parsed.breakResponses = true
		case len(fields) == 1 && fields[0] == "provider-error":
			parsed.providerError = true
		case len(fields) == 1 && fields[0] == "usage-error":
			parsed.usageError = true
		case len(fields) == 2 && fields[0] == "hold-responses":
			parsed.holdResponses[fields[1]] = true
		case len(fields) == 2 && fields[0] == "limited" && gateway.knownAlias(fields[1]):
			parsed.limited[fields[1]] = true
		case len(fields) == 3 && fields[0] == "exhaust-after" && gateway.knownAlias(fields[1]):
			// rejectAfter applies the count; here it is only validated.
			if count, err := strconv.Atoi(fields[2]); err != nil || count < 0 {
				return controls{}, fmt.Errorf("invalid control line %q", scanner.Text())
			}
		case len(fields) == 3 && fields[0] == "weekly-used" && gateway.knownAlias(fields[1]):
			percent, err := strconv.ParseFloat(fields[2], 64)
			if err != nil || percent < 0 || percent > 100 {
				return controls{}, fmt.Errorf("invalid control line %q", scanner.Text())
			}
			parsed.weeklyUsed[fields[1]] = percent
		default:
			return controls{}, fmt.Errorf("invalid control line %q", scanner.Text())
		}
	}
	return parsed, nil
}

func (gateway *gateway) knownAlias(alias string) bool {
	for _, known := range gateway.aliases {
		if known == alias {
			return true
		}
	}
	return false
}

func (gateway *gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	controls, err := gateway.readControls()
	if err != nil {
		gateway.logRequest("control-error", request, http.StatusServiceUnavailable)
		http.Error(writer, err.Error(), http.StatusServiceUnavailable)
		return
	}
	alias := gateway.alias(request)
	webSocket := request.URL.Path == responsesPath && strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	switch {
	case controls.usageError && request.URL.Path == usagePath:
		gateway.logRequest("usage-error", request, http.StatusServiceUnavailable)
		http.Error(writer, "provider unavailable (simulation)", http.StatusServiceUnavailable)
	case controls.providerError && request.URL.Path == responsesPath:
		gateway.logRequest("provider-error", request, http.StatusServiceUnavailable)
		http.Error(writer, "provider unavailable (simulation)", http.StatusServiceUnavailable)
	case controls.breakResponses && request.URL.Path == responsesPath:
		gateway.logRequest("malformed-response", request, 0)
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err == nil {
			_, _ = connection.Write([]byte("not an HTTP response\r\n\r\n"))
			_ = connection.Close()
		}
	case webSocket && controls.httpOnly:
		gateway.logRequest("websocket-refused", request, http.StatusUpgradeRequired)
		http.Error(writer, "WebSocket refused (simulation)", http.StatusUpgradeRequired)
	case controls.limited[alias] && request.URL.Path == usagePath:
		gateway.logRequest("usage-read-limited", request, http.StatusOK)
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"plan_type":"pro","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":3600,"reset_at":%d}}}`, time.Now().Add(time.Hour).Unix())
	case controls.limited[alias] && request.URL.Path == responsesPath:
		gateway.logRequest("rejected-usage-limit", request, http.StatusTooManyRequests)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusTooManyRequests)
		writer.Write(usageLimitError())
	case webSocket:
		gateway.relayWebSocket(writer, request)
	default:
		if request.URL.Path == responsesPath {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			gateway.logForwarded(request, body)
		}
		gateway.reverseProxy.ServeHTTP(writer, request)
	}
}

func usageLimitError() []byte {
	return fmt.Appendf(nil, `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"Usage limit reached (simulation)","plan_type":"pro","resets_at":%d}}`, time.Now().Add(time.Hour).Unix())
}

func (gateway *gateway) alias(request *http.Request) string {
	if alias := gateway.aliases[request.Header.Get("ChatGPT-Account-ID")]; alias != "" {
		return alias
	}
	return "unknown"
}

func (gateway *gateway) logRequest(event string, request *http.Request, status int) {
	log.Printf("event=%s method=%s path=%s account=%s thread=%q session=%q client=%q status=%d remote=%s",
		event, request.Method, request.URL.Path, gateway.alias(request), request.Header.Get("Thread-ID"),
		request.Header.Get("Session-ID"), request.Header.Get("Originator"), status, request.RemoteAddr)
}

// logForwarded records a model request on its way to OpenAI with a fingerprint
// of its exact bytes, so a test can see whether any request was sent twice.
func (gateway *gateway) logForwarded(request *http.Request, payload []byte) {
	sum := sha256.Sum256(payload)
	log.Printf("event=forwarded method=%s path=%s account=%s thread=%q session=%q client=%q request=%s remote=%s",
		request.Method, request.URL.Path, gateway.alias(request), request.Header.Get("Thread-ID"),
		request.Header.Get("Session-ID"), request.Header.Get("Originator"), hex.EncodeToString(sum[:8]), request.RemoteAddr)
}

// holdWhileControlled waits while hold-responses names this request's client.
// It returns false if the client went away first.
func (gateway *gateway) holdWhileControlled(ctx context.Context, request *http.Request) bool {
	gateway.logRequest("response-held", request, 0)
	for {
		controls, err := gateway.readControls()
		if err == nil && !controls.holdResponses[request.Header.Get("Originator")] {
			gateway.logRequest("response-released", request, 0)
			return true
		}
		select {
		case <-ctx.Done():
			gateway.logRequest("response-canceled", request, 0)
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (gateway *gateway) modifyResponse(response *http.Response) error {
	controls, err := gateway.readControls()
	if err != nil {
		return err
	}
	request := response.Request
	if percent, set := controls.weeklyUsed[gateway.alias(request)]; set && request.URL.Path == usagePath && response.StatusCode == http.StatusOK {
		return rewriteWeeklyUsedPercent(response, percent)
	}
	if request.URL.Path == responsesPath && controls.holdResponses[request.Header.Get("Originator")] {
		response.Body = &heldResponseBody{ReadCloser: response.Body, request: request, gateway: gateway}
	}
	return nil
}

// heldResponseBody delivers the first byte of a real response, then holds the
// rest, so a test can act while the work is in flight.
type heldResponseBody struct {
	io.ReadCloser
	request       *http.Request
	gateway       *gateway
	firstByteRead bool
	released      bool
}

func (body *heldResponseBody) Read(destination []byte) (int, error) {
	if !body.firstByteRead && len(destination) > 0 {
		body.firstByteRead = true
		return body.ReadCloser.Read(destination[:1])
	}
	if !body.released {
		if !body.gateway.holdWhileControlled(body.request.Context(), body.request) {
			return 0, body.request.Context().Err()
		}
		body.released = true
	}
	return body.ReadCloser.Read(destination)
}

// rewriteWeeklyUsedPercent replaces the weekly window's used percent in a real
// usage read, so a test decides which account has the most allowance left.
func rewriteWeeklyUsedPercent(response *http.Response, percent float64) error {
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return err
	}
	var usage map[string]any
	if err := json.Unmarshal(body, &usage); err != nil {
		return err
	}
	if rateLimit, ok := usage["rate_limit"].(map[string]any); ok {
		for _, name := range []string{"primary_window", "secondary_window"} {
			if window, ok := rateLimit[name].(map[string]any); ok && window["limit_window_seconds"] == float64(7*24*60*60) {
				window["used_percent"] = percent
			}
		}
	}
	if body, err = json.Marshal(usage); err != nil {
		return err
	}
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	response.Header.Del("Content-Encoding")
	return nil
}

// relayWebSocket terminates WebSocket framing so every model request on the
// connection is logged as it is forwarded. Message payloads pass unchanged.
func (gateway *gateway) relayWebSocket(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	upstreamURL := *request.URL
	upstreamURL.Scheme, upstreamURL.Host = gateway.upstream.Scheme, gateway.upstream.Host
	headers := request.Header.Clone()
	for _, name := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol"} {
		headers.Del(name)
	}
	server, response, err := websocket.Dial(ctx, upstreamURL.String(), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: gateway.transport}, HTTPHeader: headers, CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		status := http.StatusBadGateway
		if response != nil {
			status = response.StatusCode
		}
		gateway.logRequest("websocket-upstream-failed", request, status)
		http.Error(writer, "upstream WebSocket handshake failed", status)
		return
	}
	defer server.CloseNow()
	for name, values := range response.Header {
		lower := strings.ToLower(name)
		if lower != "connection" && lower != "upgrade" && !strings.HasPrefix(lower, "sec-websocket-") {
			writer.Header()[name] = values
		}
	}
	client, err := websocket.Accept(writer, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return
	}
	defer client.CloseNow()
	server.SetReadLimit(-1)
	client.SetReadLimit(-1)
	gateway.logRequest("websocket-open", request, http.StatusSwitchingProtocols)

	var activeMu sync.Mutex
	active := 0
	idle := make(chan struct{})
	close(idle)
	finished := make(chan struct{}, 2)
	go func() {
		defer func() { finished <- struct{}{} }()
		for {
			messageType, payload, err := server.Read(ctx)
			if err != nil || client.Write(ctx, messageType, payload) != nil {
				return
			}
			eventType := messageEventType(payload)
			activeMu.Lock()
			modelResponse := active > 0 // not the answer to a warm-up message
			activeMu.Unlock()
			if eventType == "response.created" && modelResponse && gateway.holding(request) && !gateway.holdWhileControlled(ctx, request) {
				return
			}
			switch eventType {
			case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "error":
				activeMu.Lock()
				if active > 0 {
					if active--; active == 0 {
						close(idle)
					}
				}
				activeMu.Unlock()
			}
		}
	}()
	go func() {
		defer func() { finished <- struct{}{} }()
		forwarded := 0
		for {
			messageType, payload, err := client.Read(ctx)
			if err != nil {
				return
			}
			if isModelRequest(payload) {
				if gateway.rejectAfter(gateway.alias(request), forwarded) {
					// A simulated limit waits for earlier responses, so it lands
					// between two model requests, as a real limit would.
					activeMu.Lock()
					previous := idle
					activeMu.Unlock()
					select {
					case <-previous:
					case <-ctx.Done():
						return
					}
					gateway.logRequest("websocket-rejected-usage-limit", request, http.StatusTooManyRequests)
					if client.Write(ctx, websocket.MessageText, usageLimitError()) != nil {
						return
					}
					continue
				}
				activeMu.Lock()
				if active == 0 {
					idle = make(chan struct{})
				}
				active++
				activeMu.Unlock()
				forwarded++
			}
			if server.Write(ctx, messageType, payload) != nil {
				return
			}
			if isModelRequest(payload) {
				gateway.logForwarded(request, payload)
			}
		}
	}()
	<-finished
	cancel()
	<-finished
}

func (gateway *gateway) holding(request *http.Request) bool {
	controls, err := gateway.readControls()
	return err == nil && controls.holdResponses[request.Header.Get("Originator")]
}

func messageEventType(payload []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &event)
	return event.Type
}

// isModelRequest excludes Codex's warm-up messages, which generate nothing.
func isModelRequest(payload []byte) bool {
	var event struct {
		Type     string `json:"type"`
		Generate *bool  `json:"generate"`
	}
	return json.Unmarshal(payload, &event) == nil && event.Type == "response.create" && (event.Generate == nil || *event.Generate)
}

// rejectAfter decides one model request. When an exhaust-after count is
// reached it records the account as limited, so usage reads agree.
func (gateway *gateway) rejectAfter(alias string, forwarded int) bool {
	gateway.controlMu.Lock()
	defer gateway.controlMu.Unlock()
	data, err := os.ReadFile(gateway.controlPath)
	if err != nil {
		return true
	}
	lines := strings.Split(string(data), "\n")
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == alias && fields[0] == "limited" {
			return true
		}
		if len(fields) == 3 && fields[1] == alias && fields[0] == "exhaust-after" {
			if count, err := strconv.Atoi(fields[2]); err == nil && forwarded >= count {
				lines[index] = "limited " + alias
				return os.WriteFile(gateway.controlPath, []byte(strings.Join(lines, "\n")), 0o600) == nil
			}
		}
	}
	return false
}

func loadAccountAliases(dataDirectory string) (map[string]string, error) {
	paths, err := filepath.Glob(filepath.Join(dataDirectory, "account-*", "auth.json"))
	if err != nil || len(paths) == 0 {
		return nil, errors.New("no account auth files found")
	}
	aliases := make(map[string]string, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var auth struct {
			Provider string `json:"provider"`
			Tokens   struct {
				AccountID string `json:"account_id"`
			} `json:"tokens"`
		}
		if json.Unmarshal(data, &auth) == nil && auth.Provider == "claude" {
			continue // Claude reaches Anthropic directly, not through this gateway
		}
		if json.Unmarshal(data, &auth) != nil || auth.Tokens.AccountID == "" {
			return nil, fmt.Errorf("%s has no account ID", path)
		}
		aliases[auth.Tokens.AccountID] = filepath.Base(filepath.Dir(path))
	}
	return aliases, nil
}
