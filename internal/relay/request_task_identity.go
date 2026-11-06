package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/joshp123/codex-switch/internal/store"
	"github.com/joshp123/codex-switch/internal/taskidentity"
)

func taskIdentityForRequest(request *http.Request) store.TaskID {
	if taskID := strings.TrimSpace(request.Header.Get(taskIDHeader)); taskID != "" {
		return store.TaskID(taskID)
	}
	if request.Header.Get("Originator") != "pi" || workKindForRequest(request) != store.ModelTurnWorkKind {
		return ""
	}
	if request.Method != http.MethodPost && !(request.Method == http.MethodGet && request.URL.Path == codexResponsesPath && strings.EqualFold(request.Header.Get("Upgrade"), "websocket")) {
		return ""
	}
	sessionID := strings.TrimSpace(request.Header.Get("Session-ID"))
	if sessionID == "" {
		return ""
	}
	return store.TaskID(taskidentity.PiSession(sessionID))
}

// forkSourceTaskForRequest is the task a Codex fork was made from, for example
// the task a suggested next message is drafted for. Codex names it in the turn
// metadata it sends with every request and WebSocket handshake; other work
// has none.
func forkSourceTaskForRequest(request *http.Request) store.TaskID {
	var metadata struct {
		ForkedFromThreadID string `json:"forked_from_thread_id"`
	}
	if json.Unmarshal([]byte(request.Header.Get("X-Codex-Turn-Metadata")), &metadata) != nil {
		return ""
	}
	return store.TaskID(strings.TrimSpace(metadata.ForkedFromThreadID))
}

// clientNameForRequest names the sending client in log lines, as the client
// names itself: the first User-Agent product token, for example
// "codex_exec/0.162.0", or its Originator header.
func clientNameForRequest(request *http.Request) string {
	if fields := strings.Fields(request.Header.Get("User-Agent")); len(fields) > 0 {
		return fields[0]
	}
	if originator := strings.TrimSpace(request.Header.Get("Originator")); originator != "" {
		return originator
	}
	return "an unnamed client"
}

// describeModelRequest names a routed model request in log lines: its task, the
// client that sent it and the account serving it.
func describeModelRequest(taskID store.TaskID, client string, accountDisplayName string) string {
	work := "task " + string(taskID)
	if taskID == "" {
		work = "request without a task ID"
	}
	if accountDisplayName == "" {
		return fmt.Sprintf("%s from %s", work, client)
	}
	return fmt.Sprintf("%s from %s on %s", work, client, accountDisplayName)
}
