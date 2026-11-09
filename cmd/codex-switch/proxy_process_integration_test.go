package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/joshp123/codex-switch/internal/clientcapability"
	"github.com/joshp123/codex-switch/internal/store"
)

func TestProxyProcessHTTPStartsAuthenticatesAndForwards(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "codex-switch")
	buildTestBinary(t, binary)
	data := t.TempDir()
	account := writeAccountFixture(
		t, filepath.Join(data, "account-1"), "http-user", "http-user@example.test", processAccessToken("http-user"),
	)
	writeAccountFixture(t, filepath.Join(data, "account-10"), "signed-out", "signed-out@example.test", "")
	claudeHome := filepath.Join(data, "account-4")
	if err := os.MkdirAll(claudeHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeHome, "auth.json"), []byte(`{"provider":"claude","account_id":"claude-user","organization_id":"organization","email":"claude@example.test","plan":"Max 20x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	turn := processTurn{
		marker: "http-smoke", task: "018f0000-0000-7000-8000-000000000201", inputTokens: 101,
	}
	upstream := newRecordingUpstream(t, map[string]int64{turn.marker: turn.inputTokens})
	proxy := startRealBinaryProxy(t, binary, data, upstream.URL, "CODEX_HOME="+root, "PI_CODING_AGENT_SESSION_DIR="+root, "CLAUDE_CONFIG_DIR="+root)
	proveUnauthenticatedClientCannotReachUpstream(t, proxy.address, upstream.dispatches)
	sendProcessTurn(t, proxy.authenticatedAddress(), turn)
	upstream.waitForUsageReads(t, 1)
	snapshot, err := fetchMenuSnapshot(data)
	deadline := time.Now().Add(10 * time.Second)
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		snapshot, err = fetchMenuSnapshot(data)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Accounts) != 3 {
		t.Fatalf("snapshot accounts: %v", snapshot.Accounts)
	}
	for index, wantNumber := range []uint32{1, 1, 2} {
		if snapshot.Accounts[index].AccountNumber != wantNumber {
			t.Fatalf("snapshot account %s has number %d, want %d", snapshot.Accounts[index].Key, snapshot.Accounts[index].AccountNumber, wantNumber)
		}
	}
	command := exec.Command(binary, "status")
	command.Env = append(os.Environ(), "CODEX_SWITCH_DATA_DIR="+data)
	statusOutput, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statusOutput)
	}
	if strings.Contains(string(statusOutput), "account-") {
		t.Fatalf("status exposes a storage key: %s", statusOutput)
	}
	for _, name := range []string{"Codex 1", "Claude 1", "Codex 2"} {
		if !bytes.Contains(statusOutput, []byte(name)) {
			t.Fatalf("status omits %s: %s", name, statusOutput)
		}
	}
	proxy.stop(t)

	select {
	case dispatch := <-upstream.dispatches:
		if dispatch.marker != turn.marker || dispatch.accountID != account.AccountID ||
			dispatch.authorization != "Bearer "+processAccessToken(account.ID) {
			t.Fatalf("HTTP dispatch = %#v", dispatch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive the HTTP turn")
	}
	logBytes, err := os.ReadFile(proxyLogPath(data))
	if err != nil {
		t.Fatal(err)
	}
	proxyLog := string(logBytes)
	for _, name := range []string{"Codex 1 (http-user@example.test, pro): available", "Claude 1 (claude@example.test, Max 20x): signed out", "Codex 2 (signed-out@example.test): signed out", "new task assigned to Codex 1"} {
		if !strings.Contains(proxyLog, name) {
			t.Fatalf("log omits %s: %s", name, proxyLog)
		}
	}
	if strings.Contains(proxyLog, "account-") {
		t.Fatalf("log exposes a storage key: %s", proxyLog)
	}
	t.Log(proxyLog)
	command = exec.Command(binary, "accounts")
	command.Env = append(os.Environ(), "CODEX_SWITCH_DATA_DIR="+data)
	listed, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(listed, []byte("Claude 1\tclaude:claude@example.test\n")) || !bytes.Contains(listed, []byte("Codex 2\tchatgpt:signed-out@example.test\n")) {
		t.Fatalf("accounts: %v\n%s", err, listed)
	}
	database, err := store.OpenStoreDatabase(filepath.Join(data, "codex-switch.db"))
	if err != nil {
		t.Fatal(err)
	}
	pin, found, err := database.ReadTaskAccountPin(store.TaskID(turn.task))
	database.CloseStoreDatabase()
	if err != nil || !found || pin.AccountKey != "chatgpt:http-user@example.test" {
		t.Fatalf("task pin: %v, %v, %v", pin, found, err)
	}
	// Keep the test's discarded credentials in its temporary directory.
	trashExecutable := filepath.Join(root, "trash")
	if err := os.WriteFile(trashExecutable, []byte("#!/bin/sh\nexec /bin/mv \"$1\" \"$CODEX_SWITCH_TEST_TRASH\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"Claude 1", "chatgpt:signed-out@example.test"} {
		command = exec.Command(binary, "remove", target)
		command.Env = append(os.Environ(), "CODEX_SWITCH_DATA_DIR="+data, "PATH="+root+":"+os.Getenv("PATH"), "CODEX_SWITCH_TEST_TRASH="+filepath.Join(root, target))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("remove %s: %v\n%s", target, err, output)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "account-1", "auth.json")); err != nil {
		t.Fatalf("remove changed another account: %v", err)
	}

}

func TestProxyProcessReusesWebSocket(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "codex-switch")
	buildTestBinary(t, binary)
	data := t.TempDir()
	account := writeAccountFixture(
		t, filepath.Join(data, "account-1"), "ws-user", "ws-user@example.test", processAccessToken("ws-user"),
	)
	taskA := "018f0000-0000-7000-8000-000000000301"
	turns := []processTurn{
		{marker: "ws-first", task: taskA, inputTokens: 201},
		{marker: "ws-second", task: taskA, inputTokens: 202},
	}
	upstream := newWebSocketRecordingUpstream(t, turns)
	proxy := startRealBinaryProxy(t, binary, data, upstream.URL)
	connection := dialProcessWebSocket(t, proxy.authenticatedAddress(), taskA, "turn")
	upstream.waitForUsageReads(t, 1)
	for _, turn := range turns {
		runProcessWebSocketTurn(t, connection, turn)
	}
	connection.CloseNow()
	proxy.stop(t)

	facts := make([]webSocketTurnFact, len(turns))
	for index, turn := range turns {
		select {
		case facts[index] = <-upstream.turns:
		case <-time.After(2 * time.Second):
			t.Fatalf("upstream did not record WebSocket turn %q", turn.marker)
		}
		if facts[index].marker != turn.marker || facts[index].accountID != account.AccountID ||
			facts[index].authorization != "Bearer "+processAccessToken(account.ID) {
			t.Fatalf("WebSocket dispatch %d = %#v", index+1, facts[index])
		}
	}
	if facts[0].connection != facts[1].connection {
		t.Fatalf("turns used connections %d and %d", facts[0].connection, facts[1].connection)
	}
}

type webSocketTurnFact struct {
	connection            int64
	marker, authorization string
	accountID             string
}

type webSocketRecordingUpstream struct {
	*httptest.Server
	usage       chan struct{}
	turns       chan webSocketTurnFact
	responses   map[string]processTurn
	connections atomic.Int64
}

func newWebSocketRecordingUpstream(t *testing.T, turns []processTurn) *webSocketRecordingUpstream {
	t.Helper()
	upstream := &webSocketRecordingUpstream{
		usage: make(chan struct{}, 8), turns: make(chan webSocketTurnFact, len(turns)+1),
		responses: make(map[string]processTurn, len(turns)),
	}
	for _, turn := range turns {
		upstream.responses[turn.marker] = turn
	}
	upstream.Server = httptest.NewServer(http.HandlerFunc(upstream.serveHTTP))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *webSocketRecordingUpstream) serveHTTP(output http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/backend-api/wham/usage" {
		u.usage <- struct{}{}
		output.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(output, weeklyAllowanceReply())
		return
	}
	connection, err := websocket.Accept(output, request, nil)
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connectionID := u.connections.Add(1)
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	for {
		kind, message, readErr := connection.Read(ctx)
		if readErr != nil {
			return
		}
		var create struct {
			Type  string `json:"type"`
			Input string `json:"input"`
		}
		if kind != websocket.MessageText || json.Unmarshal(message, &create) != nil || create.Type != "response.create" {
			return
		}
		turn, exists := u.responses[create.Input]
		if !exists {
			return
		}
		created := webSocketCreatedFrame(turn.marker)
		completed := webSocketCompletedFrame(turn)
		u.turns <- webSocketTurnFact{
			connection: connectionID, marker: turn.marker,
			authorization: request.Header.Get("Authorization"), accountID: request.Header.Get("ChatGPT-Account-ID"),
		}
		if connection.Write(ctx, websocket.MessageText, created) != nil ||
			connection.Write(ctx, websocket.MessageText, completed) != nil {
			return
		}
	}
}

func (u *webSocketRecordingUpstream) waitForUsageReads(t *testing.T, want int) {
	t.Helper()
	for range want {
		select {
		case <-u.usage:
		case <-time.After(5 * time.Second):
			t.Fatalf("WebSocket upstream usage reads did not reach %d", want)
		}
	}
}

func dialProcessWebSocket(t *testing.T, address, task, kind string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws://"+address+"/backend-api/codex/responses", &websocket.DialOptions{
		HTTPHeader: processWebSocketHeaders(task, kind), CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	return connection
}

func runProcessWebSocketTurn(t *testing.T, connection *websocket.Conn, turn processTurn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := webSocketCreateFrame(turn.marker)
	if err := connection.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{webSocketCreatedFrame(turn.marker), webSocketCompletedFrame(turn)} {
		kind, got, err := connection.Read(ctx)
		if err != nil || kind != websocket.MessageText || !bytes.Equal(got, want) {
			t.Fatalf("WebSocket frame for %q = %d %s, %v", turn.marker, kind, got, err)
		}
	}
}

func processWebSocketHeaders(task, kind string) http.Header {
	session := "018f0000-0000-7000-8000-000000000303"
	metadata, _ := json.Marshal(map[string]string{
		"installation_id": "process-websocket-smoke", "session_id": session,
		"thread_id": task, "window_id": "process-websocket-window", "request_kind": kind,
	})
	return http.Header{
		"Thread-ID": {task}, "Session-ID": {session},
		"OpenAI-Beta": {"responses_websockets=2026-02-06"}, "X-Codex-Turn-Metadata": {string(metadata)},
	}
}

func webSocketCreateFrame(marker string) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.create","input":%q}`, marker))
}

func webSocketCreatedFrame(marker string) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, "response-"+marker))
}

func webSocketCompletedFrame(turn processTurn) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":"gpt-5.6-luna","service_tier":"priority","usage":{"input_tokens":%d,"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":1},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":%d}}}`,
		"response-"+turn.marker, turn.inputTokens, turn.inputTokens+3))
}

type processTurn struct {
	marker, task string
	inputTokens  int64
}

type upstreamDispatch struct {
	marker, authorization, accountID string
}

type recordingUpstream struct {
	*httptest.Server
	usage      chan struct{}
	dispatches chan upstreamDispatch
	responses  map[string]int64
}

func proveUnauthenticatedClientCannotReachUpstream(
	t *testing.T,
	address string,
	upstreamDispatches <-chan upstreamDispatch,
) {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		"http://"+address+"/backend-api/codex/responses",
		bytes.NewBufferString(`{"input":"unauthenticated","stream":false}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("unauthenticated client response = %d, %v", response.StatusCode, readErr)
	}
	select {
	case dispatch := <-upstreamDispatches:
		t.Fatalf("unauthenticated client reached upstream: %#v", dispatch)
	default:
	}
}

func newRecordingUpstream(t *testing.T, responses map[string]int64) *recordingUpstream {
	t.Helper()
	upstream := &recordingUpstream{
		usage: make(chan struct{}, 64), dispatches: make(chan upstreamDispatch, len(responses)+1),
		responses: responses,
	}
	upstream.Server = httptest.NewServer(http.HandlerFunc(upstream.serveHTTP))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *recordingUpstream) serveHTTP(output http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/backend-api/wham/usage" {
		u.usage <- struct{}{}
		output.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(output, weeklyAllowanceReply())
		return
	}
	var input struct {
		Input string `json:"input"`
	}
	if request.URL.Path != "/backend-api/codex/responses" || json.NewDecoder(request.Body).Decode(&input) != nil {
		http.Error(output, "invalid request", http.StatusBadRequest)
		return
	}
	inputTokens, ok := u.responses[input.Input]
	if ok {
		u.dispatches <- upstreamDispatch{
			marker: input.Input, authorization: request.Header.Get("Authorization"),
			accountID: request.Header.Get("ChatGPT-Account-ID"),
		}
	}
	if !ok {
		http.Error(output, "unknown turn", http.StatusBadRequest)
		return
	}
	output.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(output, `{"id":%q,"object":"response","status":"completed","model":"gpt-5.6-luna","service_tier":"priority","output":[],"usage":{"input_tokens":%d,"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":1},"output_tokens":3,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":%d}}`,
		"response-"+input.Input, inputTokens, inputTokens+3)
}

func (u *recordingUpstream) waitForUsageReads(t *testing.T, want int) {
	t.Helper()
	for range want {
		select {
		case <-u.usage:
		case <-time.After(5 * time.Second):
			t.Fatalf("upstream usage reads did not reach %d", want)
		}
	}
}

func sendProcessTurn(t *testing.T, address string, turn processTurn) {
	t.Helper()
	requestBody, _ := json.Marshal(map[string]any{"input": turn.marker, "stream": false})
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/backend-api/codex/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	session := "018f0000-0000-7000-8000-000000000201"
	metadata, _ := json.Marshal(map[string]string{
		"installation_id": "process-smoke", "session_id": session,
		"thread_id": turn.task, "window_id": "process-window", "request_kind": "turn",
	})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Thread-ID", turn.task)
	request.Header.Set("Session-ID", session)
	request.Header.Set("X-Codex-Turn-Metadata", string(metadata))
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"id":"response-`+turn.marker+`"`)) {
		t.Fatalf("turn %q response = %d %s, %v", turn.marker, response.StatusCode, body, readErr)
	}
}

func processAccessToken(subject string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := []byte(fmt.Sprintf(`{"exp":4102444800,"sub":%q}`, subject))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// buildTestBinary compiles the command under test; it replaces a helper that
// lived in the deleted blue/green acceptance file.
func buildTestBinary(t *testing.T, binary string) {
	t.Helper()
	command := exec.Command("go", "build", "-o", binary, ".")
	command.Dir = "."
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
}

type realBinaryProxyProcess struct {
	command          *exec.Cmd
	done             chan error
	stderr           bytes.Buffer
	address          string
	clientCapability string
	stopped          bool
}

func startRealBinaryProxy(t *testing.T, binary, data, upstream string, extraEnv ...string) *realBinaryProxyProcess {
	t.Helper()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatalf("protect temporary proxy data directory: %v", err)
	}
	process := &realBinaryProxyProcess{done: make(chan error, 1)}
	process.command = exec.Command(binary, "serve", "--listen", "127.0.0.1:0")
	process.command.Env = append(os.Environ(),
		append([]string{
			"CODEX_SWITCH_DATA_DIR=" + data,
			"CODEX_SWITCH_UPSTREAM_ORIGIN=" + upstream,
			"CODEX_SWITCH_USAGE_URL=" + upstream + "/usage",
		}, extraEnv...)...,
	)
	stdout, err := process.command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	process.command.Stderr = &process.stderr
	if err := process.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { process.done <- process.command.Wait() }()
	t.Cleanup(func() { process.stop(t) })

	type startupResult struct {
		line string
		err  error
	}
	started := make(chan startupResult, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			started <- startupResult{line: scanner.Text()}
			return
		}
		started <- startupResult{err: scanner.Err()}
	}()
	var line string
	select {
	case result := <-started:
		if result.line == "" {
			t.Fatalf("temporary proxy did not report its address: %v\n%s", result.err, process.stderr.Bytes())
		}
		line = result.line
	case err := <-process.done:
		process.stopped = true
		t.Fatalf("temporary proxy exited during startup: %v\n%s", err, process.stderr.Bytes())
	case <-time.After(5 * time.Second):
		t.Fatalf("temporary proxy startup timed out\n%s", process.stderr.Bytes())
	}
	const prefix = "codex-switch listening on http://"
	process.address = strings.TrimPrefix(line, prefix)
	if process.address == line {
		t.Fatalf("unexpected temporary proxy output: %q", line)
	}
	process.clientCapability, err = clientcapability.Read(data)
	if err != nil {
		process.stop(t)
		t.Fatalf("read temporary proxy client capability: %v", err)
	}
	return process
}

func (process *realBinaryProxyProcess) authenticatedAddress() string {
	return process.address + clientcapability.PathPrefix(process.clientCapability)
}

func (process *realBinaryProxyProcess) stop(t *testing.T) {
	t.Helper()
	if process.stopped {
		return
	}
	_ = process.command.Process.Signal(os.Interrupt)
	select {
	case err := <-process.done:
		process.stopped = true
		if err != nil {
			t.Errorf("stop temporary proxy: %v\n%s", err, process.stderr.Bytes())
		}
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		select {
		case <-process.done:
			process.stopped = true
			t.Errorf("temporary proxy required forced termination\n%s", process.stderr.Bytes())
		case <-time.After(5 * time.Second):
			t.Errorf("temporary proxy did not exit after forced termination\n%s", process.stderr.Bytes())
		}
	}
}

// weeklyAllowanceReply is wham/usage for a Pro account with most of its week
// left, as OpenAI reports it.
func weeklyAllowanceReply() string {
	return fmt.Sprintf(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":%d}}}`,
		time.Now().Add(4*24*time.Hour).Unix())
}
