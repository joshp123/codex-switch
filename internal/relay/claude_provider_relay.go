package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
)

type ClaudeAccountObserver interface {
	ObserveClaudeResponse(store.AccountName, string, http.Header, int)
}

func prepareClaudeHeaders(headers http.Header) {
	headers.Del("X-Api-Key")
	headers.Del(chatGPTAccountIDHeader)
	if headers.Get("Anthropic-Version") == "" {
		headers.Set("Anthropic-Version", "2023-06-01")
	}
	for _, beta := range []string{"oauth-2025-04-20", "claude-code-20250219"} {
		found := false
		for _, existing := range strings.Split(headers.Get("Anthropic-Beta"), ",") {
			if strings.TrimSpace(existing) == beta {
				found = true
			}
		}
		if !found {
			value := headers.Get("Anthropic-Beta")
			if value != "" {
				value += ","
			}
			headers.Set("Anthropic-Beta", value+beta)
		}
	}
}

func (handler *TrafficRelayHandler) observeClaudeRejection(request *routedRelayRequest, response *http.Response) error {
	if request.workKind != store.ModelTurnWorkKind {
		return nil
	}
	request.responseObserver.settleModelRequest("", store.ModelRequestFailed)
	request.responseObserver.admittedRequests = 0
	switch response.StatusCode {
	case http.StatusTooManyRequests:
		head := peekResponseBody(response)
		limited := describeModelRequest(request.taskID, request.client, request.routeDecision.DisplayName) + ": Anthropic limited " + request.modelSlug +
			" (" + describeClaudeLimitHeaders(response.Header) + ")" + describeRejection(response.Header, head)
		answerClaudeRejection(response, handler.taskRouter, request.modelSlug, limited)
	case http.StatusUnauthorized:
		handler.taskRouter.ReportUnauthorizedTurn(request.routeDecision.AccountName, request.routeDecision.Credentials)
		response.StatusCode = http.StatusServiceUnavailable
		response.Status = "503 Service Unavailable"
		response.Header.Set("Retry-After", "1")
		response.Header.Set("X-Should-Retry", "true")
	default:
		head := peekResponseBody(response)
		log.Printf("%s: Anthropic answered %s with HTTP %d%s", describeModelRequest(request.taskID, request.client, request.routeDecision.DisplayName), request.modelSlug, response.StatusCode, describeRejection(response.Header, head))
	}
	return nil
}

// answerClaudeRejection decides what Claude Code sees of Anthropic's 429. No
// rejected request is replayed: the client's next request chooses from the
// whole pool. Claude Code stops at a 429 that names a usage limit, so while an
// account can take the work now, or after a throttle, the client gets a 503 it
// retries at once. When only a usage limit's reset frees an account, it gets
// that limit and the pool's earliest reset, which T3 resumes at.
func answerClaudeRejection(response *http.Response, router TaskRouter, modelSlug, limited string) {
	accountFree, availableAgainAt, limitClaim, err := router.ClaudePoolCapacity(modelSlug)
	if err != nil {
		log.Printf("%s; could not read the pool: %v", limited, err)
		return
	}
	switch {
	case accountFree || !availableAgainAt.IsZero() && limitClaim == "":
		for name := range response.Header {
			if strings.HasPrefix(strings.ToLower(name), "anthropic-ratelimit-") {
				response.Header.Del(name)
			}
		}
		response.StatusCode, response.Status = http.StatusServiceUnavailable, "503 Service Unavailable"
		response.Header.Set("Retry-After", "1")
		response.Header.Set("X-Should-Retry", "true")
		if accountFree {
			log.Printf("%s; the client's retry goes to another account", limited)
		} else {
			log.Printf("%s; the client's retry waits for a throttle to end at %s", limited, availableAgainAt.Local().Format("15:04:05"))
		}
	case !availableAgainAt.IsZero():
		availableAgain := availableAgainAt.Local().Format("Mon 2 Jan 15:04")
		response.Header.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		response.Header.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(availableAgainAt.Unix(), 10))
		response.Header.Set("Anthropic-Ratelimit-Unified-Representative-Claim", limitClaim)
		log.Printf("%s; no account can take it, so the client was told its %s limit until %s", limited, limitClaim, availableAgain)
	default:
		// No reset frees an account, for example every account is signed out.
		log.Printf("%s; no reset frees an account", limited)
	}
}

// peekResponseBody reads only the start of the body for the reason; the client
// gets every byte.
func peekResponseBody(response *http.Response) []byte {
	head, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), response.Body), response.Body}
	return head
}

// describeClaudeLimitHeaders lists the unified rate-limit headers on a 429, so
// the log shows which limit Anthropic named.
func describeClaudeLimitHeaders(header http.Header) string {
	var named []string
	for name, values := range header {
		name = strings.ToLower(name)
		if strings.HasPrefix(name, "anthropic-ratelimit-unified-") || name == "retry-after" {
			named = append(named, strings.TrimPrefix(name, "anthropic-ratelimit-unified-")+"="+strings.Join(values, ","))
		}
	}
	if len(named) == 0 {
		return "no rate-limit headers"
	}
	slices.Sort(named)
	return strings.Join(named, " ")
}

func (observer *terminalResponseObserver) observeClaudeUsage(payload []byte) {
	complete, err := observer.claudeUsage.Observe(payload)
	if err != nil {
		log.Printf("%s: read Claude usage: %v", observer.describe(), err)
		return
	}
	if !complete {
		return
	}
	record, err := observer.claudeUsage.Record(observer.taskID, observer.accountName, time.Now().UTC())
	if err != nil {
		log.Printf("%s: price Claude usage: %v", observer.describe(), err)
		return
	}
	observer.settleModelRequest("", store.ModelRequestSuccessful)
	observer.admittedRequests = 0
	if err := observer.completionLedger.RecordCompletedResponse(record); err != nil {
		log.Printf("%s: record Claude usage: %v", observer.describe(), err)
	}
}

// claudeCapacityWaitLimit is how long Claude model work waits for capacity.
// Claude Code abandons a request after about three minutes without response
// headers and reports a timeout, which hides the limit from it and from T3.
const claudeCapacityWaitLimit = time.Minute

// answerExhaustedClaudePool refuses Claude work no account can take. When an
// allowance reset will free an account, the refusal is the 429 Anthropic sends
// a limited subscription: Claude Code stops retrying, reports the reset, and
// T3 can resume the turn then. It returns what the client was told, or "" while
// waiting is still worth it.
func answerExhaustedClaudePool(response http.ResponseWriter, exhaustion error, waited time.Duration) string {
	var exhausted *routing.ClaudePoolExhaustedError
	if !errors.As(exhaustion, &exhausted) {
		exhausted = &routing.ClaudePoolExhaustedError{AccountProblems: exhaustion.Error()}
	}
	untilAvailable := time.Until(exhausted.AvailableAgainAt)
	if untilAvailable > 0 && (untilAvailable > claudeCapacityWaitLimit || waited >= claudeCapacityWaitLimit) {
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		response.Header().Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(exhausted.AvailableAgainAt.Unix(), 10))
		availableAgain := exhausted.AvailableAgainAt.Local().Format("Mon 2 Jan 15:04")
		told := "Anthropic throttles every account until " + availableAgain
		if exhausted.LimitClaim != "" {
			// Without a claim Claude Code treats the 429 as a throttle and retries.
			response.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", exhausted.LimitClaim)
			told = "its " + exhausted.LimitClaim + " limit until " + availableAgain
		}
		writeClaudeError(response, http.StatusTooManyRequests, "rate_limit_error", "codex-switch: no Claude account can take this work until "+availableAgain+": "+exhausted.AccountProblems)
		return told
	}
	if waited < claudeCapacityWaitLimit {
		return ""
	}
	// No reset frees an account, so retrying cannot help.
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-Should-Retry", "false")
	writeClaudeError(response, http.StatusServiceUnavailable, "api_error", "codex-switch: no Claude account can take this work: "+exhausted.AccountProblems)
	return "no allowance reset frees an account"
}

func writeClaudeError(response http.ResponseWriter, status int, errorType, message string) {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": errorType, "message": message}})
	response.WriteHeader(status)
	_, _ = response.Write(body)
}
