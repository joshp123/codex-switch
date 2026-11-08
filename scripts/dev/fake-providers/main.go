// fake-providers stands in for OpenAI and Anthropic in the system test. It
// answers every endpoint the proxy and the clients use, from a scripted model,
// so the test spends no tokens and waits on no real service. A control file
// sets what a test cannot otherwise arrange: allowance levels, exhausted
// accounts, throttles, refused WebSockets and held or broken responses. Every
// model request is logged with a fingerprint of its bytes, so a test can see
// that none was sent twice. It is never part of a live proxy.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

const (
	responsesPath      = "/backend-api/codex/responses"
	usagePath          = "/backend-api/wham/usage"
	modelsPath         = "/backend-api/codex/models"
	accountsCheckPath  = "/backend-api/wham/accounts/check"
	claudeMessagesPath = "/v1/messages"
	claudeUsagePath    = "/api/oauth/usage"
	claudeTokenPath    = "/v1/oauth/token"
)

// codexModels is OpenAI's catalogue entry for gpt-6-luna without its
// instruction text, so Codex offers the tools it offers in real use.
//
//go:embed codex-models.json
var codexModels []byte

func main() {
	dataDirectory := flag.String("data-dir", "", "proxy data directory containing account-N/auth.json")
	controlPath := flag.String("control", "", "simulation file; an empty file means every account is available")
	authorityPath := flag.String("certificate-authority", "", "where to write the PEM certificate that the HTTPS listener's certificate chains to")
	requestsDirectory := flag.String("requests", "", "directory that keeps a copy of every model request, for reading what a client sent")
	flag.Parse()
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if *dataDirectory == "" || *controlPath == "" || *authorityPath == "" {
		log.Fatal("data-dir, control and certificate-authority are required")
	}
	aliases, err := loadAccountAliases(*dataDirectory)
	if err != nil {
		log.Fatal(err)
	}
	providers := &providers{controlPath: *controlPath, aliases: aliases, requestsDirectory: *requestsDirectory}
	if _, err := providers.readControls(); err != nil {
		log.Fatal(err)
	}
	// The proxy uses HTTP. Codex sends its own ChatGPT requests, such as its
	// account check, only to an HTTPS origin; the test has Codex trust the
	// certificate authority written here.
	certificate, err := writeCertificateAuthority(*authorityPath)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal("cannot listen on loopback")
	}
	secureListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal("cannot listen on loopback")
	}
	fmt.Printf("http://%s https://%s\n", listener.Addr(), secureListener.Addr())
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0), Handler: providers}
	go func() {
		secure := &http.Server{ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0), Handler: providers}
		if err := secure.Serve(tls.NewListener(secureListener, &tls.Config{Certificates: []tls.Certificate{certificate}})); err != nil {
			log.Fatal("fake providers stopped unexpectedly")
		}
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("fake providers stopped unexpectedly")
	}
}

// writeCertificateAuthority makes a certificate authority for this run, writes
// its certificate to path, and returns a certificate for 127.0.0.1 it signed.
func writeCertificateAuthority(path string) (tls.Certificate, error) {
	authorityKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	authority := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Codex Switch system test"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	authorityDER, err := x509.CreateCertificate(rand.Reader, authority, authority, &authorityKey.PublicKey, authorityKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	authorityCertificate, err := x509.ParseCertificate(authorityDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, authorityCertificate, &key.PublicKey, authorityKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authorityDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, authorityDER}, PrivateKey: key}, nil
}

type providers struct {
	controlPath       string
	requestsDirectory string
	aliases           map[string]string // ChatGPT account ID, or claudeAlias of a Claude access token, to account-N
	controlMu         sync.Mutex
	sequence          atomic.Int64
	// conversations holds where each response left its conversation, by
	// response ID: Codex sends only the new items with previous_response_id.
	conversations sync.Map
	resetClaims   sync.Map
	resetAccounts sync.Map
}

// controls is one reading of the simulation file:
//
//	weekly-used account-N PERCENT   usage reads report PERCENT of the weekly window used
//	weekly-resets account-N UNIX    the weekly window resets at UNIX seconds
//	weekly-idle account-N           the weekly window has not started: nothing used, a reset a full week from each read
//	limited account-N               the account's allowance has run out
//	exhaust-after account-N COUNT   each WebSocket gets COUNT model requests, then the allowance runs out
//	http-only                       WebSocket upgrades are refused, so clients use HTTP
//	hold-responses CLIENT           model responses for CLIENT (its Originator header) pause once started
//	break-responses                 model requests receive a malformed HTTP response
//	provider-error                  model requests receive a provider HTTP error
//	usage-error                     usage reads receive a provider HTTP error
//	claude-throttle COUNT           the next COUNT Claude model requests receive a 429 that names no usage limit
//	claude-limited account-N        the Claude account's five-hour allowance has run out
//	websocket-age-limit SECONDS     a WebSocket opened now gets OpenAI's one-hour limit error after SECONDS, then closes
//
// Fast-mode Claude requests are always refused, as for an account without usage credits.
// Claude login refreshes are always refused, as for a refresh token Anthropic has retired.
type controls struct {
	weeklyUsed        map[string]float64
	weeklyResets      map[string]int64
	weeklyIdle        map[string]bool
	limited           map[string]bool
	httpOnly          bool
	holdResponses     map[string]bool
	breakResponses    bool
	providerError     bool
	usageError        bool
	claudeThrottle    bool
	claudeLimited     map[string]bool
	resetTest         bool
	resetSoon         bool
	resetDrop         bool
	resetRefuse       bool
	claudeGrant       bool
	webSocketAgeLimit time.Duration
}

func (providers *providers) readControls() (controls, error) {
	providers.controlMu.Lock()
	defer providers.controlMu.Unlock()
	data, err := os.ReadFile(providers.controlPath)
	if err != nil {
		return controls{}, errors.New("cannot read simulation control file")
	}
	parsed := controls{weeklyUsed: map[string]float64{}, weeklyResets: map[string]int64{}, weeklyIdle: map[string]bool{}, limited: map[string]bool{}, holdResponses: map[string]bool{}, claudeLimited: map[string]bool{}}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 0:
		case len(fields) == 1 && fields[0] == "reset-test":
			parsed.resetTest = true
		case len(fields) == 1 && fields[0] == "reset-soon":
			parsed.resetSoon = true
		case len(fields) == 1 && fields[0] == "reset-drop":
			parsed.resetDrop = true
		case len(fields) == 1 && fields[0] == "reset-refuse":
			parsed.resetRefuse = true
		case len(fields) == 1 && fields[0] == "claude-grant":
			parsed.claudeGrant = true
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
		case len(fields) == 2 && fields[0] == "weekly-idle" && providers.knownAlias(fields[1]):
			parsed.weeklyIdle[fields[1]] = true
		case len(fields) == 3 && fields[0] == "weekly-resets" && providers.knownAlias(fields[1]):
			resetAt, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || resetAt <= 0 {
				return controls{}, fmt.Errorf("invalid control line %q", scanner.Text())
			}
			parsed.weeklyResets[fields[1]] = resetAt
		case len(fields) == 2 && fields[0] == "limited" && providers.knownAlias(fields[1]):
			parsed.limited[fields[1]] = true
		case len(fields) == 2 && fields[0] == "claude-limited" && providers.knownAlias(fields[1]):
			parsed.claudeLimited[fields[1]] = true
		case len(fields) == 2 && fields[0] == "claude-throttle":
			// takeCountedControl applies the count; here it is only validated.
			count, err := strconv.Atoi(fields[1])
			if err != nil || count < 0 {
				return controls{}, fmt.Errorf("invalid control line %q", scanner.Text())
			}
			parsed.claudeThrottle = count > 0
		case len(fields) == 2 && fields[0] == "websocket-age-limit":
			seconds, err := strconv.ParseFloat(fields[1], 64)
			if err != nil || seconds <= 0 {
				return controls{}, fmt.Errorf("invalid control line %q", scanner.Text())
			}
			parsed.webSocketAgeLimit = time.Duration(seconds * float64(time.Second))
		case len(fields) == 3 && fields[0] == "exhaust-after" && providers.knownAlias(fields[1]):
			if count, err := strconv.Atoi(fields[2]); err != nil || count < 0 {
				return controls{}, fmt.Errorf("invalid control line %q", scanner.Text())
			}
		case len(fields) == 3 && fields[0] == "weekly-used" && providers.knownAlias(fields[1]):
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

func (providers *providers) knownAlias(alias string) bool {
	for _, known := range providers.aliases {
		if known == alias {
			return true
		}
	}
	return false
}

func (providers *providers) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	controls, err := providers.readControls()
	if err != nil {
		providers.logRequest("control-error", request, http.StatusServiceUnavailable)
		http.Error(writer, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/v1/") || strings.HasPrefix(request.URL.Path, "/api/") {
		providers.serveAnthropic(writer, request, controls)
		return
	}
	alias := providers.alias(request)
	webSocket := request.URL.Path == responsesPath && strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	switch {
	case request.URL.Path == modelsPath:
		writeJSON(writer, http.StatusOK, codexModels)
	case request.URL.Path == accountsCheckPath:
		writeJSON(writer, http.StatusOK, accountsCheck(request.Header.Get("ChatGPT-Account-ID")))
	case request.URL.Path == "/backend-api/wham/rate-limit-reset-credits" && controls.resetTest:
		expiry := 7200
		if alias == "account-2" {
			expiry = 3600
		}
		status := "available"
		if _, redeemed := providers.resetAccounts.Load(alias); redeemed {
			status = "redeemed"
		}
		body := fmt.Appendf(nil, `{"credits":[{"id":%q,"status":%q,"reset_type":"codex_rate_limits","expires_at":%q}]}`, "credit-"+alias, status, time.Now().Add(time.Duration(expiry)*time.Second).Format(time.RFC3339))
		writeJSON(writer, http.StatusOK, body)
	case request.URL.Path == "/backend-api/wham/rate-limit-reset-credits/consume" && controls.resetTest:
		var claim struct {
			CreditID  string `json:"credit_id"`
			RequestID string `json:"redeem_request_id"`
		}
		_ = json.NewDecoder(request.Body).Decode(&claim)
		if controls.resetRefuse {
			log.Printf("event=reset-refused account=%s credit=%s", alias, claim.CreditID)
			http.Error(writer, "provider unavailable (simulation)", http.StatusServiceUnavailable)
			return
		}
		log.Printf("event=reset-claim account=%s credit=%s request=%s", alias, claim.CreditID, claim.RequestID)
		_, already := providers.resetClaims.LoadOrStore(claim.RequestID, true)
		providers.resetAccounts.Store(alias, true)
		if controls.resetDrop && !already {
			if connection, _, err := writer.(http.Hijacker).Hijack(); err == nil {
				_ = connection.Close()
			}
			return
		}
		if already {
			writeJSON(writer, http.StatusOK, []byte(`{"code":"already_redeemed"}`))
		} else {
			writeJSON(writer, http.StatusOK, []byte(`{"code":"reset"}`))
		}
	case request.URL.Path == usagePath && controls.usageError:
		providers.logRequest("usage-error", request, http.StatusServiceUnavailable)
		http.Error(writer, "provider unavailable (simulation)", http.StatusServiceUnavailable)
	case request.URL.Path == "/backend-api/wham/rate-limit-reset-credits":
		writeJSON(writer, http.StatusOK, []byte(`{"credits":[{"id":"test-credit","status":"available","reset_type":"codex_rate_limits","expires_at":"2099-01-01T00:00:00Z"}]}`))
	case request.URL.Path == usagePath:
		if _, reset := providers.resetAccounts.Load(alias); reset {
			delete(controls.limited, alias)
		}
		used, body := openAIUsage(controls, alias)
		log.Printf("event=usage-read account=%s used=%g", alias, used)
		writeJSON(writer, http.StatusOK, body)
	case request.URL.Path != responsesPath:
		providers.logRequest("not-found", request, http.StatusNotFound)
		http.NotFound(writer, request)
	case controls.providerError:
		providers.logRequest("provider-error", request, http.StatusServiceUnavailable)
		http.Error(writer, "provider unavailable (simulation)", http.StatusServiceUnavailable)
	case controls.breakResponses:
		providers.logRequest("malformed-response", request, 0)
		if connection, _, err := writer.(http.Hijacker).Hijack(); err == nil {
			_, _ = connection.Write([]byte("not an HTTP response\r\n\r\n"))
			_ = connection.Close()
		}
	case webSocket && controls.httpOnly:
		providers.logRequest("websocket-refused", request, http.StatusUpgradeRequired)
		http.Error(writer, "WebSocket refused (simulation)", http.StatusUpgradeRequired)
	case controls.limited[alias] && !providers.accountReset(alias):
		providers.logRequest("rejected-usage-limit", request, http.StatusTooManyRequests)
		writeJSON(writer, http.StatusTooManyRequests, usageLimitError())
	case webSocket:
		providers.serveCodexWebSocket(writer, request, controls.webSocketAgeLimit)
	default:
		providers.serveCodexHTTP(writer, request)
	}
}

func (providers *providers) accountReset(alias string) bool {
	_, reset := providers.resetAccounts.Load(alias)
	return reset
}

func writeJSON(writer http.ResponseWriter, status int, body []byte) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func openAIUsage(controls controls, alias string) (float64, []byte) {
	used, limited := 10.0, controls.limited[alias]
	if level, set := controls.weeklyUsed[alias]; set {
		used = level
	}
	if limited {
		used = 100
	}
	now := time.Now().Unix()
	resetAt := now + 3600
	if controls.resetSoon && alias == "account-2" {
		resetAt = now + 300
	}
	if !limited {
		resetAt = now + 4*86400
		if at, set := controls.weeklyResets[alias]; set {
			resetAt = at
		}
		if controls.weeklyIdle[alias] {
			used, resetAt = 0, now+604800
		}
	}
	return used, fmt.Appendf(nil, `{"plan_type":"pro","rate_limit":{"allowed":%t,"limit_reached":%t,"primary_window":{"used_percent":%g,"limit_window_seconds":604800,"reset_after_seconds":%d,"reset_at":%d}}}`,
		!limited, limited, used, resetAt-now, resetAt)
}

// accountsCheck is the account Codex asks about, as ChatGPT describes a
// personal Pro account that may work anywhere.
func accountsCheck(accountID string) []byte {
	account := map[string]any{"id": accountID, "account_user_id": accountID + "-user", "account_user_role": "account-owner",
		"structure": "personal", "plan_type": "pro", "is_zdr": false, "is_openai_internal": false,
		"tbo_config":               map[string]any{"plan_eligible": false, "tbo_available": false, "waitlist_info": "NO_WAITLIST"},
		"account_residency_region": "no_constraint", "can_access_with_session": true, "enable_account_switching": false,
		"is_deactivated": false, "is_fedramp_compliant_workspace": false, "workspace_backend_origin": "NO_CONSTRAINT",
		"account_routing_override": "NO_CONSTRAINT", "name": nil, "profile_picture_url": nil, "sso_connection_name": nil}
	encoded, _ := json.Marshal(map[string]any{"accounts": []any{account}, "default_account_id": accountID, "account_ordering": []string{accountID}})
	return encoded
}

// webSocketAgeLimitError is what OpenAI sends when a Responses WebSocket has
// been open an hour, as Codex's own tests reproduce it.
func webSocketAgeLimitError() []byte {
	return []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue."}}`)
}

func usageLimitError() []byte {
	return fmt.Appendf(nil, `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"Usage limit reached (simulation)","plan_type":"pro","resets_at":%d}}`, time.Now().Add(time.Hour).Unix())
}

func (providers *providers) alias(request *http.Request) string {
	if alias := providers.aliases[request.Header.Get("ChatGPT-Account-ID")]; alias != "" {
		return alias
	}
	if alias := providers.aliases[claudeAlias(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))]; alias != "" {
		return alias
	}
	return "unknown"
}

func (providers *providers) logRequest(event string, request *http.Request, status int) {
	log.Printf("event=%s method=%s path=%s account=%s thread=%q session=%q client=%q status=%d remote=%s",
		event, request.Method, request.URL.Path, providers.alias(request), request.Header.Get("Thread-ID"),
		request.Header.Get("Session-ID"), request.Header.Get("Originator"), status, request.RemoteAddr)
}

// logForwarded records a model request with a fingerprint of its exact bytes,
// so a test can see whether any request was sent twice.
func (providers *providers) logForwarded(request *http.Request, payload []byte) {
	sum := sha256.Sum256(payload)
	if providers.requestsDirectory != "" {
		_ = os.WriteFile(filepath.Join(providers.requestsDirectory, hex.EncodeToString(sum[:8])+".json"), payload, 0o600)
	}
	log.Printf("event=forwarded method=%s path=%s account=%s thread=%q session=%q client=%q request=%s remote=%s",
		request.Method, request.URL.Path, providers.alias(request), request.Header.Get("Thread-ID"),
		request.Header.Get("Session-ID"), request.Header.Get("Originator"), hex.EncodeToString(sum[:8]), request.RemoteAddr)
}

// holdWhileControlled waits while hold-responses names this request's client.
// It returns false if the client went away first.
func (providers *providers) holdWhileControlled(ctx context.Context, request *http.Request) bool {
	providers.logRequest("response-held", request, 0)
	for {
		controls, err := providers.readControls()
		if err == nil && !controls.holdResponses[request.Header.Get("Originator")] {
			providers.logRequest("response-released", request, 0)
			return true
		}
		select {
		case <-ctx.Done():
			providers.logRequest("response-canceled", request, 0)
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (providers *providers) holding(request *http.Request) bool {
	controls, err := providers.readControls()
	return err == nil && controls.holdResponses[request.Header.Get("Originator")]
}

// takeCountedControl uses up one request of a counted control line, such as
// claude-throttle, or reports that an exhaust-after count is reached and marks
// the account limited, so usage reads agree.
func (providers *providers) takeCountedControl(name, alias string, forwarded int) bool {
	providers.controlMu.Lock()
	defer providers.controlMu.Unlock()
	data, err := os.ReadFile(providers.controlPath)
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	for index, line := range lines {
		fields := strings.Fields(line)
		switch {
		case name == "exhaust-after" && len(fields) == 2 && fields[0] == "limited" && fields[1] == alias:
			return true
		case name == "exhaust-after" && len(fields) == 3 && fields[0] == name && fields[1] == alias:
			if count, err := strconv.Atoi(fields[2]); err == nil && forwarded >= count {
				lines[index] = "limited " + alias
				return os.WriteFile(providers.controlPath, []byte(strings.Join(lines, "\n")), 0o600) == nil
			}
		case name == "claude-throttle" && len(fields) == 2 && fields[0] == name:
			if count, err := strconv.Atoi(fields[1]); err == nil && count > 0 {
				lines[index] = name + " " + strconv.Itoa(count-1)
				return os.WriteFile(providers.controlPath, []byte(strings.Join(lines, "\n")), 0o600) == nil
			}
		}
	}
	return false
}

// The scripted model

// A prompt scripts the model in lines it acts on one per model request:
//
//	RUN: <shell command>    a tool call that runs the command
//	GOAL COMPLETE           a tool call that marks the thread's goal complete
//	REPLY: <text>           the final answer, which ends the turn
//
// A prompt without script lines gets the word after "Reply with the word",
// or "ok".
var replyWithWord = regexp.MustCompile(`(?i)reply with the (?:word|number) ([\w-]+)`)

type modelStep struct {
	command      string
	goalComplete bool
	reply        string
}

// nextStep is what the model does now: the script's next step, given how many
// tool results have come back since the script was given.
func nextStep(prompt string, toolResults int) modelStep {
	var steps []modelStep
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "RUN: "):
			steps = append(steps, modelStep{command: strings.TrimPrefix(line, "RUN: ")})
		case line == "GOAL COMPLETE":
			steps = append(steps, modelStep{goalComplete: true})
		case strings.HasPrefix(line, "REPLY: "):
			steps = append(steps, modelStep{reply: strings.TrimPrefix(line, "REPLY: ")})
		}
	}
	if len(steps) == 0 {
		if match := replyWithWord.FindStringSubmatch(prompt); match != nil {
			return modelStep{reply: match[1]}
		}
		return modelStep{reply: "ok"}
	}
	if toolResults < len(steps) {
		return steps[toolResults]
	}
	return modelStep{reply: "done"}
}

// Codex

type codexRequest struct {
	Type               string            `json:"type"`
	Model              string            `json:"model"`
	Generate           *bool             `json:"generate"`
	PreviousResponseID string            `json:"previous_response_id"`
	Input              []json.RawMessage `json:"input"`
}

// conversation is where a conversation stands: the script it follows and the
// tool results since that script was given, or its latest user text when it
// has no script.
type conversation struct {
	prompt, script string
	results        int
}

// conversationFor continues the conversation a request belongs to with the
// items it sends. Codex repeats a goal's objective in every turn that
// continues it, so a repeated script keeps its count; a new script, such as a
// resumed task's next prompt, starts again.
func (providers *providers) conversationFor(request codexRequest) conversation {
	var current conversation
	if previous, found := providers.conversations.Load(request.PreviousResponseID); found && request.PreviousResponseID != "" {
		current = previous.(conversation)
	}
	for _, raw := range request.Input {
		var item struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		switch {
		case item.Type == "message" && item.Role == "user":
			var text strings.Builder
			for _, part := range item.Content {
				text.WriteString(html.UnescapeString(part.Text) + "\n")
			}
			lines := scriptLines(text.String())
			switch {
			case lines != "" && lines != current.script:
				current.prompt, current.script, current.results = text.String(), lines, 0
			case lines == "" && current.script == "" && (replyWithWord.MatchString(text.String()) || !replyWithWord.MatchString(current.prompt)):
				// Codex sends instructions and context as user messages too.
				current.prompt = text.String()
			}
		case strings.HasSuffix(item.Type, "_call_output"):
			current.results++
		}
	}
	return current
}

func scriptLines(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "RUN: ") || strings.HasPrefix(line, "REPLY: ") || line == "GOAL COMPLETE" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// codexOutputItem is the model's output for one step. gpt-6-luna's catalogue
// entry makes Codex offer its tools only through exec, the code tool, which is
// how the real model runs commands and completes a goal.
func codexOutputItem(step modelStep, number int64) map[string]any {
	if step.reply != "" {
		return map[string]any{"type": "message", "id": fmt.Sprintf("msg_fake_%d", number), "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": step.reply, "annotations": []any{}}}}
	}
	code := "const r = await tools.update_goal({status:\"complete\"});\ntext(JSON.stringify(r))\n"
	if !step.goalComplete {
		encoded, _ := json.Marshal(step.command)
		code = "const r = await tools.exec_command({cmd:" + string(encoded) + ",yield_time_ms:60000,max_output_tokens:1000});\ntext(JSON.stringify(r))\n"
	}
	return map[string]any{"type": "custom_tool_call", "id": fmt.Sprintf("ctc_fake_%d", number), "status": "completed",
		"call_id": fmt.Sprintf("call_fake_%d", number), "name": "exec", "input": code}
}

// codexEvents are the Responses API events for one model request. A warm-up
// request, which generates nothing, completes with no output.
func (providers *providers) codexEvents(payload []byte) []map[string]any {
	var request codexRequest
	_ = json.Unmarshal(payload, &request)
	number := providers.sequence.Add(1)
	responseID := fmt.Sprintf("resp_fake_%d", number)
	output := []any{}
	current := providers.conversationFor(request)
	providers.conversations.Store(responseID, current)
	if request.Generate == nil || *request.Generate {
		output = append(output, codexOutputItem(nextStep(current.prompt, current.results), number))
	}
	input := int64(len(payload)/4 + 1)
	usage := map[string]any{"input_tokens": input, "input_tokens_details": map[string]any{"cached_tokens": input / 2},
		"output_tokens": 7, "output_tokens_details": map[string]any{"reasoning_tokens": 2}, "total_tokens": input + 7}
	// A warm-up's response says it generated nothing, as OpenAI's does.
	created := map[string]any{"id": responseID, "model": request.Model, "status": "in_progress", "output": []any{}}
	completed := map[string]any{"id": responseID, "model": request.Model, "status": "completed", "output": output, "usage": usage}
	if request.Generate != nil && !*request.Generate {
		created["generate"], completed["generate"] = false, false
	}
	events := []map[string]any{{"type": "response.created", "response": created}}
	for index, item := range output {
		events = append(events, map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	return append(events, map[string]any{"type": "response.completed",
		"response": completed})
}

func isModelRequest(payload []byte) bool {
	var event codexRequest
	return json.Unmarshal(payload, &event) == nil && event.Type == "response.create" && (event.Generate == nil || *event.Generate)
}

func (providers *providers) serveCodexHTTP(writer http.ResponseWriter, request *http.Request) {
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		return
	}
	providers.logForwarded(request, payload)
	// Codex compresses the requests it sends over HTTP.
	if strings.EqualFold(request.Header.Get("Content-Encoding"), "zstd") {
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			http.Error(writer, "cannot decode zstd", http.StatusInternalServerError)
			return
		}
		payload, err = decoder.DecodeAll(payload, nil)
		decoder.Close()
		if err != nil {
			http.Error(writer, "request body is not zstd", http.StatusBadRequest)
			return
		}
	}
	events := providers.codexEvents(payload)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	for index, event := range events {
		encoded, _ := json.Marshal(event)
		fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event["type"], encoded)
		if flusher != nil {
			flusher.Flush()
		}
		if index == 0 && providers.holding(request) && !providers.holdWhileControlled(request.Context(), request) {
			return
		}
	}
}

// serveCodexWebSocket answers each response.create message on the connection.
// Messages are read on their own goroutine, so a client that goes away cancels
// a held reply. A simulated limit lands between two model requests, as a real
// limit would.
func (providers *providers) serveCodexWebSocket(writer http.ResponseWriter, request *http.Request, ageLimit time.Duration) {
	client, err := websocket.Accept(writer, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(-1)
	providers.logRequest("websocket-open", request, http.StatusSwitchingProtocols)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	messages := make(chan []byte)
	go func() {
		defer cancel()
		for {
			_, payload, err := client.Read(ctx)
			if err != nil {
				return
			}
			select {
			case messages <- payload:
			case <-ctx.Done():
				return
			}
		}
	}()
	// OpenAI ends a connection at its age limit even while it is idle, as it
	// did to Codex's idle connections in October 2026.
	var ageLimitReached <-chan time.Time
	if ageLimit > 0 {
		ageLimitReached = time.After(ageLimit)
	}
	forwarded := 0
	for {
		var payload []byte
		select {
		case payload = <-messages:
		case <-ageLimitReached:
			providers.logRequest("websocket-age-limit", request, http.StatusBadRequest)
			_ = client.Write(ctx, websocket.MessageText, webSocketAgeLimitError())
			client.Close(websocket.StatusNormalClosure, "")
			return
		case <-ctx.Done():
			return
		}
		if isModelRequest(payload) {
			if providers.takeCountedControl("exhaust-after", providers.alias(request), forwarded) {
				providers.logRequest("websocket-rejected-usage-limit", request, http.StatusTooManyRequests)
				if client.Write(ctx, websocket.MessageText, usageLimitError()) != nil {
					return
				}
				continue
			}
			forwarded++
			providers.logForwarded(request, payload)
		}
		for index, event := range providers.codexEvents(payload) {
			encoded, _ := json.Marshal(event)
			if client.Write(ctx, websocket.MessageText, encoded) != nil {
				return
			}
			if index == 0 && isModelRequest(payload) && providers.holding(request) && !providers.holdWhileControlled(ctx, request) {
				return
			}
		}
	}
}

// Anthropic

func (providers *providers) serveAnthropic(writer http.ResponseWriter, request *http.Request, controls controls) {
	alias := providers.alias(request)
	switch {
	case request.URL.Path == claudeTokenPath:
		providers.logRequest("claude-refresh-refused", request, http.StatusBadRequest)
		writeJSON(writer, http.StatusBadRequest, []byte(`{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`))
	case request.URL.Path == claudeUsagePath && controls.claudeGrant:
		writeJSON(writer, http.StatusOK, fmt.Appendf(nil, `{"five_hour":{"utilization":10,"resets_at":%q},"cedar_ember":{"eligible":true,"grants":[{"resets_left":2},{"resets_left":1,"ends_at":%q}]}}`, time.Now().Add(3*time.Hour).Format(time.RFC3339), time.Now().Add(-time.Hour).Format(time.RFC3339)))
	case request.URL.Path == claudeUsagePath && controls.claudeLimited[alias]:
		providers.logRequest("claude-usage-read-limited", request, http.StatusOK)
		writeJSON(writer, http.StatusOK, fmt.Appendf(nil, `{"five_hour":{"utilization":100,"resets_at":%q}}`, claudeLimitReset().Format(time.RFC3339)))
	case request.URL.Path == claudeUsagePath:
		writeJSON(writer, http.StatusOK, fmt.Appendf(nil, `{"five_hour":{"utilization":10,"resets_at":%q},"seven_day":{"utilization":20,"resets_at":%q}}`,
			time.Now().Add(3*time.Hour).Format(time.RFC3339), time.Now().Add(4*24*time.Hour).Format(time.RFC3339)))
	case request.URL.Path == "/v1/messages/count_tokens":
		writeJSON(writer, http.StatusOK, []byte(`{"input_tokens":10}`))
	case request.URL.Path != claudeMessagesPath || request.Method != http.MethodPost:
		providers.logRequest("claude-other", request, http.StatusOK)
		writeJSON(writer, http.StatusOK, []byte(`{}`))
	default:
		payload, _ := io.ReadAll(request.Body)
		var message claudeRequest
		_ = json.Unmarshal(payload, &message)
		switch {
		case controls.claudeLimited[alias]:
			providers.logRequest("claude-rejected-usage-limit", request, http.StatusTooManyRequests)
			reset := strconv.FormatInt(claudeLimitReset().Unix(), 10)
			writeClaudeRejection(writer, map[string]string{"status": "rejected", "reset": reset, "representative-claim": "five_hour",
				"5h-status": "rejected", "5h-utilization": "1.0", "5h-reset": reset}, "Rate limited (simulation)")
		case message.Speed == "fast":
			// What Anthropic answers an account without usage credits, which pay
			// for fast mode on a subscription.
			providers.logRequest("claude-fast-mode-refused", request, http.StatusTooManyRequests)
			writeClaudeRejection(writer, map[string]string{"overage-disabled-reason": "org_level_disabled",
				"reset": strconv.FormatInt(time.Now().AddDate(0, 1, 0).Unix(), 10)}, "Usage credits are required for fast mode.")
		case controls.claudeThrottle && providers.takeCountedControl("claude-throttle", alias, 0):
			// What Anthropic sent on 31 October 2026: a 429 naming no exhausted
			// window and no limit, with a reset hours away that matched neither
			// the account's five-hour nor its weekly window.
			providers.logRequest("claude-throttled", request, http.StatusTooManyRequests)
			writer.Header().Set("Retry-After", "1")
			writeClaudeRejection(writer, map[string]string{"status": "rejected", "reset": strconv.FormatInt(time.Now().Add(7*time.Hour).Unix(), 10),
				"5h-status": "allowed", "5h-utilization": "0.15", "5h-reset": strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10)}, "Rate limited (simulation)")
		default:
			providers.logRequest("claude-forwarded", request, http.StatusOK)
			providers.serveClaudeMessage(writer, message, len(payload))
		}
	}
}

type claudeRequest struct {
	Model    string `json:"model"`
	Speed    string `json:"speed"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// claudePrompt is the text of the latest user message.
func (request claudeRequest) claudePrompt() string {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		message := request.Messages[index]
		if message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			return text
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(message.Content, &parts)
		var joined strings.Builder
		for _, part := range parts {
			if part.Type == "text" {
				joined.WriteString(part.Text + "\n")
			}
		}
		return joined.String()
	}
	return ""
}

func (providers *providers) serveClaudeMessage(writer http.ResponseWriter, request claudeRequest, size int) {
	reply := nextStep(request.claudePrompt(), 0).reply
	if reply == "" {
		reply = "ok"
	}
	input := size/4 + 1
	events := []struct {
		name string
		data map[string]any
	}{
		{"message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_fake_%d", providers.sequence.Add(1)), "type": "message",
			"role": "assistant", "model": request.Model, "content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": input, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 1, "speed": "standard"}}}},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": reply}}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
		{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
			"usage": map[string]any{"input_tokens": input, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 3}}},
		{"message_stop", map[string]any{"type": "message_stop"}},
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	writer.WriteHeader(http.StatusOK)
	var body bytes.Buffer
	for _, event := range events {
		encoded, _ := json.Marshal(event.data)
		fmt.Fprintf(&body, "event: %s\ndata: %s\n\n", event.name, encoded)
	}
	_, _ = writer.Write(body.Bytes())
}

func claudeAlias(accessToken string) string {
	if accessToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(accessToken))
	return "claude:" + hex.EncodeToString(sum[:8])
}

// claudeLimitReset is an hour from now, as usageLimitError's reset is.
func claudeLimitReset() time.Time { return time.Now().Add(time.Hour).Truncate(time.Second) }

func writeClaudeRejection(writer http.ResponseWriter, limits map[string]string, message string) {
	for name, value := range limits {
		writer.Header().Set("Anthropic-Ratelimit-Unified-"+name, value)
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "rate_limit_error", "message": message}})
	writeJSON(writer, http.StatusTooManyRequests, body)
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
			Provider    string `json:"provider"`
			AccessToken string `json:"access_token"`
			Tokens      struct {
				AccountID string `json:"account_id"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(data, &auth); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		alias := filepath.Base(filepath.Dir(path))
		if auth.Provider == "claude" {
			// Claude requests carry no account ID; their access tokens never change.
			aliases[claudeAlias(auth.AccessToken)] = alias
		} else if auth.Tokens.AccountID != "" {
			aliases[auth.Tokens.AccountID] = alias
		}
	}
	return aliases, nil
}
