package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/providerreply"
	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"

	"github.com/joshp123/codex-switch/internal/logtime"
)

// limitNumber is a number in a Claude limit header: a time, a count of
// seconds or a share used.
var limitNumber = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,6})?$`)

type ClaudeAccountObserver interface {
	ObserveClaudeResponse(store.AccountKey, string, http.Header, int)
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
			" with " + providerreply.Refusal(response.StatusCode, head) + " (" + describeClaudeLimitHeaders(response.Header) + ")"
		answerClaudeRejection(response, handler.taskRouter, request.modelSlug, limited)
	case http.StatusUnauthorized:
		handler.taskRouter.ReportUnauthorizedTurn(request.routeDecision.AccountKey, request.routeDecision.Credentials)
		response.StatusCode = http.StatusServiceUnavailable
		response.Status = "503 Service Unavailable"
		response.Header.Set("Retry-After", "1")
		response.Header.Set("X-Should-Retry", "true")
	default:
		head := peekResponseBody(response)
		log.Printf("%s: Anthropic answered %s with %s", describeModelRequest(request.taskID, request.client, request.routeDecision.DisplayName), request.modelSlug, providerreply.Refusal(response.StatusCode, head))
	}
	return nil
}

// passClaudeFastModeRefusal hands Claude Code Anthropic's 429 for a fast-mode
// request unchanged when it names no usage limit. Subscriptions pay for fast
// mode only from usage credits, so an account without them refuses it while it
// has allowance left. Claude Code then retries at standard speed, on the same
// account and its prompt cache, so the account is not held back. A usage limit
// on a fast-mode request is answered as any other.
func (handler *TrafficRelayHandler) passClaudeFastModeRefusal(request *routedRelayRequest, response *http.Response) error {
	if request.workKind == store.ModelTurnWorkKind {
		request.responseObserver.settleModelRequest("", store.ModelRequestFailed)
		request.responseObserver.admittedRequests = 0
	}
	head := peekResponseBody(response)
	log.Printf("%s: Anthropic refused fast mode for %s with %s (%s); Claude Code retries at standard speed on the same account",
		describeModelRequest(request.taskID, request.client, request.routeDecision.DisplayName), request.modelSlug,
		providerreply.Refusal(response.StatusCode, head), describeClaudeLimitHeaders(response.Header))
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
			log.Printf("%s; the client's retry waits for a throttle to end at %s", limited, logtime.Format(availableAgainAt))
		}
	case !availableAgainAt.IsZero():
		availableAgain := logtime.Format(availableAgainAt)
		response.Header.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		response.Header.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(availableAgainAt.Unix(), 10))
		response.Header.Set("Anthropic-Ratelimit-Unified-Representative-Claim", limitClaim)
		log.Printf("%s; no account can take it, so the client was told its %s limit until %s", limited, limitClaim, availableAgain)
	default:
		// No reset frees an account, for example every account is signed out.
		log.Printf("%s; no reset frees an account", limited)
	}
}

// peekResponseBody reads only the start of the body for its error codes; the
// client gets every byte.
func peekResponseBody(response *http.Response) []byte {
	head, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), response.Body), response.Body}
	return head
}

// describeClaudeLimitHeaders lists the unified rate-limit headers on a 429, so
// the log shows which limit Anthropic named. Their values are codes, such as
// five_hour, and numbers; a header with any other value is left out.
func describeClaudeLimitHeaders(header http.Header) string {
	var named []string
	for name, values := range header {
		name = strings.ToLower(name)
		if !strings.HasPrefix(name, "anthropic-ratelimit-unified-") && name != "retry-after" {
			continue
		}
		name = strings.TrimPrefix(name, "anthropic-ratelimit-unified-")
		value := strings.Join(values, ",")
		if providerreply.Code(value) != "" || limitNumber.MatchString(value) {
			named = append(named, name+"="+value)
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
	record, err := observer.claudeUsage.Record(observer.taskID, observer.accountKey, time.Now().UTC())
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
		availableAgain := logtime.Format(exhausted.AvailableAgainAt)
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
