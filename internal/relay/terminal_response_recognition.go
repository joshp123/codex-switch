package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
	"github.com/joshp123/codex-switch/internal/usage"
)

const providerUsageLimitErrorType = "usage_limit_reached"

type terminalResponseObserver struct {
	claudeUsage *usage.ClaudeResponseUsage
	taskID      store.TaskID
	// forkSourceTaskID is the task a Codex fork was made from. Its responses
	// count toward that task, as the fork has no name or history of its own.
	forkSourceTaskID   store.TaskID
	client             string
	accountName        store.AccountName
	accountDisplayName string
	taskRouter         TaskRouter
	completionLedger   CompletionLedger
	activeResponses    int
	admittedRequests   int
	modelSlug          string
	// serviceTier is the speed the client requested. OpenAI's completed
	// response reports "default" even for Fast work it bills as Fast.
	serviceTier     string
	admissionMutex  sync.Mutex
	requestCounters ModelRequestCounters
	onFailure       func()
	onModelRequest  func()
	requests        []*observedModelRequest

	// A WebSocket's state during an update; see in_flight_request_drain.go.
	closeWebSocket       func()
	idleSince            time.Time
	closingForUpdate     bool
	updateCloseScheduled bool
	webSocketClosed      bool
}

type completedResponseWire struct {
	Status   string             `json:"status"`
	Generate *bool              `json:"generate"`
	ID       string             `json:"id"`
	Model    string             `json:"model"`
	Error    *providerErrorWire `json:"error"`
	// IncompleteDetails says why OpenAI ended a response incomplete, for example
	// "max_output_tokens".
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *struct {
		InputTokens       *int64 `json:"input_tokens"`
		InputTokenDetails struct {
			CachedTokens     int64  `json:"cached_tokens"`
			CacheWriteTokens *int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		OutputTokens       *int64 `json:"output_tokens"`
		OutputTokenDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

type structuredResponseEvent struct {
	Type     string                `json:"type"`
	Response completedResponseWire `json:"response"`
	Error    *providerErrorWire    `json:"error"`
}

type providerErrorWire struct {
	Type     string `json:"type"`
	Message  string `json:"message"`
	ResetsAt int64  `json:"resets_at"`
}

type httpJSONResponseWire struct {
	completedResponseWire
	Error  *providerErrorWire `json:"error"`
	Detail string             `json:"detail"`
}

func newTerminalResponseObserver(
	taskID store.TaskID,
	client string,
	accountName store.AccountName,
	taskRouter TaskRouter,
	completionLedger CompletionLedger,
) *terminalResponseObserver {
	return &terminalResponseObserver{
		taskID: taskID, client: client, accountName: accountName,
		taskRouter: taskRouter, completionLedger: completionLedger,
	}
}

func (observer *terminalResponseObserver) observeStructuredEvent(eventBytes []byte) bool {
	observer.admissionMutex.Lock()
	defer observer.admissionMutex.Unlock()
	if observer.claudeUsage != nil {
		observer.observeClaudeUsage(eventBytes)
		return false
	}
	var event structuredResponseEvent
	if json.Unmarshal(eventBytes, &event) != nil {
		return false
	}
	switch event.Type {
	case "response.created":
		for _, request := range observer.requests {
			if !request.settled && request.responseID == "" {
				request.responseID = event.Response.ID
				break
			}
		}
		observer.activeResponses++
		return false
	case "response.completed":
		if event.Response.ID != "" {
			observer.settleModelRequest(event.Response.ID, store.ModelRequestSuccessful)
		}
		observer.admittedRequests = max(0, observer.admittedRequests-1)
		observer.activeResponses = max(0, observer.activeResponses-1)
		observer.recordCompletedResponse(event.Response)
		observer.noteResponseEnded()
		return false
	case "response.failed", "response.incomplete":
		observer.settleModelRequest(event.Response.ID, store.ModelRequestFailed)
		observer.admittedRequests = max(0, observer.admittedRequests-1)
		observer.logProviderFailure(event.Response.failureReason())
		observer.activeResponses = max(0, observer.activeResponses-1)
		observer.noteResponseEnded()
		return false
	case "error":
		observer.settleModelRequest("", store.ModelRequestFailed)
		reconnect := observer.logProviderFailure(event.Error)
		if observer.activeResponses == 0 && observer.admittedRequests <= 1 {
			observer.admittedRequests = max(0, observer.admittedRequests-1)
			// A WebSocket handshake carries no model, so reconnecting helps only
			// when the task's pin moved to an account that offers the model.
			reconnect = (observer.reportUnsupportedModel(event.Error) && observer.taskID != "") || reconnect
		}
		observer.noteResponseEnded()
		return reconnect && observer.activeResponses == 0 && observer.admittedRequests == 0
	default:
		return false
	}
}

func (observer *terminalResponseObserver) observeHTTPJSONBody(bodyBytes []byte) {
	observer.admissionMutex.Lock()
	defer observer.admissionMutex.Unlock()
	if observer.claudeUsage != nil {
		observer.observeClaudeUsage(bodyBytes)
		return
	}
	var response httpJSONResponseWire
	if json.Unmarshal(bodyBytes, &response) != nil {
		return
	}
	if response.Error != nil {
		observer.settleModelRequest(response.ID, store.ModelRequestFailed)
		observer.admittedRequests = 0
		observer.logProviderFailure(response.Error)
		return
	}
	if response.Status == "failed" || response.Status == "incomplete" {
		observer.settleModelRequest(response.ID, store.ModelRequestFailed)
		observer.admittedRequests = 0
		observer.logProviderFailure(response.failureReason())
		return
	}
	if response.ID != "" && (response.Status == "completed" || response.Usage != nil) {
		observer.settleModelRequest(response.ID, store.ModelRequestSuccessful)
		observer.admittedRequests = 0
	}
	if response.Usage != nil {
		observer.recordCompletedResponse(response.completedResponseWire)
	}
}

// failureReason is OpenAI's error for a failed response, or the reason it ended
// the response incomplete.
func (response completedResponseWire) failureReason() *providerErrorWire {
	if response.Error == nil && response.IncompleteDetails != nil {
		return &providerErrorWire{Type: "incomplete", Message: response.IncompleteDetails.Reason}
	}
	return response.Error
}

// logProviderFailure logs a model response OpenAI failed, with its reason, and
// reports a usage limit to routing. It returns whether a usage limit moved the work.
func (observer *terminalResponseObserver) logProviderFailure(providerError *providerErrorWire) bool {
	subject := observer.describe()
	if limitOutcome, limited := observer.reportProviderUsageLimit(providerError); limited {
		log.Printf("%s: %s", subject, limitOutcome)
		return true
	}
	reason := ""
	if providerError != nil {
		reason = describeProviderReason(httpJSONResponseWire{Error: providerError})
	}
	log.Printf("%s: OpenAI failed the %s response%s", subject, observer.modelSlug, reason)
	return false
}

func (observer *terminalResponseObserver) describe() string {
	return describeModelRequest(observer.taskID, observer.client, observer.accountDisplayName)
}

func (observer *terminalResponseObserver) logReadError(err error) {
	observer.admissionMutex.Lock()
	unfinished := observer.activeResponses > 0 || observer.admittedRequests > 0
	modelSlug := observer.modelSlug
	closedByProxy := observer.webSocketClosed
	observer.admissionMutex.Unlock()
	if closedByProxy {
		return
	}
	// Codex closes the stream once a response completes; that is not a failure.
	if !unfinished && (errors.Is(err, io.EOF) || errors.Is(err, context.Canceled)) {
		return
	}
	switch {
	case errors.Is(err, io.EOF):
		provider := "OpenAI"
		if observer.claudeUsage != nil {
			provider = "Anthropic"
		}
		log.Printf("%s: %s closed the stream before the %s response completed; the proxy did not resend it", observer.describe(), provider, modelSlug)
	case errors.Is(err, context.Canceled):
		log.Printf("%s: the client closed the stream before the %s response completed", observer.describe(), modelSlug)
	default:
		log.Printf("%s: reading the %s response stream failed: %v; the proxy did not resend it", observer.describe(), modelSlug, err)
	}
}

// reportProviderUsageLimit records an account's exhaustion and moves the task.
// It returns what happened, for the proxy log.
func (observer *terminalResponseObserver) reportProviderUsageLimit(providerError *providerErrorWire) (string, bool) {
	if providerError == nil || providerError.Type != providerUsageLimitErrorType || providerError.ResetsAt == 0 {
		return "", false
	}
	return observer.taskRouter.ReportExhaustedTurn(
		observer.taskID,
		observer.accountName,
		time.Unix(providerError.ResetsAt, 0).UTC(),
	), true
}

func (observer *terminalResponseObserver) recordCompletedResponse(response completedResponseWire) {
	if response.Generate != nil && !*response.Generate {
		return
	}
	// Codex requires response.id when decoding response.completed: https://github.com/openai/codex/blob/400ee190c30d5e4a88549c070a2335311f0baa91/codex-rs/codex-api/src/sse/responses.rs#L112-L120
	if response.ID == "" || response.Model == "" || response.Usage == nil ||
		response.Usage.InputTokens == nil || response.Usage.OutputTokens == nil {
		log.Printf("%s: OpenAI completed response %q without complete token usage; its usage is not recorded", observer.describe(), response.ID)
		return
	}
	taskID := observer.taskID
	if observer.forkSourceTaskID != "" {
		taskID = observer.forkSourceTaskID
	}
	record := store.CompletedResponseRecord{
		CompletedResponseID:   store.CompletedResponseID(response.ID),
		TaskID:                taskID,
		AccountName:           observer.accountName,
		ModelSlug:             response.Model,
		HarnessMode:           harnessModeForServiceTier(observer.serviceTier),
		InputTokens:           *response.Usage.InputTokens,
		CachedInputTokens:     response.Usage.InputTokenDetails.CachedTokens,
		CacheWriteInputTokens: response.Usage.InputTokenDetails.CacheWriteTokens,
		OutputTokens:          *response.Usage.OutputTokens,
		ReasoningTokens:       response.Usage.OutputTokenDetails.ReasoningTokens,
		CompletedAt:           time.Now().UTC(),
	}
	if err := observer.completionLedger.RecordCompletedResponse(record); err != nil {
		log.Printf("%s: could not record the usage of response %q: %v", observer.describe(), response.ID, err)
	}
}

// harnessModeForServiceTier maps the requested service tier to the Codex
// speed: Codex requests Fast as "priority".
func harnessModeForServiceTier(serviceTier string) string {
	if serviceTier == "priority" || serviceTier == "fast" {
		return "fast"
	}
	return "standard"
}

func (observer *terminalResponseObserver) routeClientMessage(message []byte) bool {
	var request struct {
		Type        string `json:"type"`
		Model       string `json:"model"`
		ServiceTier string `json:"service_tier"`
		Generate    *bool  `json:"generate"`
	}
	if json.Unmarshal(message, &request) != nil || request.Type != "response.create" {
		return false
	}
	if observer.onModelRequest != nil {
		observer.onModelRequest()
	}
	observer.admissionMutex.Lock()
	defer observer.admissionMutex.Unlock()
	// A concurrent create cannot tear down work already admitted on this socket.
	if observer.admittedRequests > 0 || observer.activeResponses > 0 {
		observer.startModelRequest(request.Generate != nil && !*request.Generate)
		observer.admittedRequests++
		return false
	}
	// Closing before forwarding makes the client send this request again on a
	// new connection, to the new process.
	if observer.closingForUpdate {
		observer.webSocketClosed = true
		log.Printf("%s: the WebSocket closes for the update before its next %s request; the client sends it on a new connection", observer.describe(), request.Model)
		return true
	}
	// A socket without a task ID keeps the account chosen at its handshake: no
	// pin would carry a different choice across a reconnect.
	if observer.taskID != "" {
		decision, err := observer.taskRouter.RouteForTask(observer.taskID, store.ModelTurnWorkKind, request.Model)
		if err != nil {
			if !errors.Is(err, routing.ErrNoAvailableAccount) && observer.onFailure != nil {
				observer.onFailure()
			}
			log.Printf("%s: the socket closes so the client reconnects, because routing its next %s request failed: %v", observer.describe(), request.Model, err)
			return true
		}
		if decision.Assignment != "" {
			log.Printf("%s: %s", describeModelRequest(observer.taskID, observer.client, ""), decision.Assignment)
		}
		if decision.AccountName != observer.accountName {
			// Closing the socket before forwarding makes Codex reconnect and send
			// this message again, now routed to the chosen account.
			return true
		}
	}
	observer.modelSlug = request.Model
	observer.serviceTier = request.ServiceTier
	observer.startModelRequest(request.Generate != nil && !*request.Generate)
	observer.admittedRequests++
	return false
}

func (observer *terminalResponseObserver) reportUnsupportedModel(providerError *providerErrorWire) bool {
	if providerError == nil || providerError.Type != "invalid_request_error" || observer.modelSlug == "" {
		return false
	}
	expected := "The '" + observer.modelSlug + "' model is not supported when using Codex with a ChatGPT account."
	if strings.TrimSpace(providerError.Message) != expected {
		return false
	}
	return observer.taskRouter.ReportUnsupportedModel(observer.taskID, observer.accountName, observer.modelSlug)
}

func (observer *terminalResponseObserver) reportUnsupportedModelDetail(detail string) bool {
	return observer.reportUnsupportedModel(&providerErrorWire{Type: "invalid_request_error", Message: detail})
}
