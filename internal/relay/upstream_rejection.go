package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/joshp123/codex-switch/internal/providerreply"
	"github.com/joshp123/codex-switch/internal/store"
)

// observeRejectedResponse logs every request OpenAI answers with HTTP 400 or
// above, with its status and error codes. A model request rejected before OpenAI
// started work because its account is out of allowance, or does not offer the
// model, moves its task and becomes a retryable error for Codex; the proxy never
// resends the request itself. Every other rejection reaches the client unchanged.
func (handler *TrafficRelayHandler) observeRejectedResponse(routedRequest *routedRelayRequest, upstreamResponse *http.Response) error {
	subject := describeModelRequest(routedRequest.taskID, routedRequest.client, routedRequest.routeDecision.DisplayName)
	if routedRequest.workKind != store.ModelTurnWorkKind {
		// Read only the start of the body for its error codes, then hand the
		// client every byte unchanged.
		head, _ := io.ReadAll(io.LimitReader(upstreamResponse.Body, 4096))
		upstreamResponse.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), upstreamResponse.Body), upstreamResponse.Body}
		log.Printf("%s: OpenAI answered %s %s with %s", subject, upstreamResponse.Request.Method, upstreamResponse.Request.URL.Path, describeRefusal(upstreamResponse.Header, upstreamResponse.StatusCode, head))
		return nil
	}
	if upstreamResponse.StatusCode == http.StatusUnauthorized {
		handler.taskRouter.ReportUnauthorizedTurn(routedRequest.routeDecision.AccountKey, routedRequest.routeDecision.Credentials)
	}
	responseObserver := routedRequest.responseObserver
	responseObserver.settleModelRequest("", store.ModelRequestFailed)
	responseObserver.admittedRequests = 0

	body, err := io.ReadAll(upstreamResponse.Body)
	upstreamResponse.Body.Close()
	if err != nil {
		log.Printf("%s: OpenAI rejected %s with HTTP %d and its body could not be read: %s", subject, routedRequest.modelSlug, upstreamResponse.StatusCode, providerreply.Failure(err))
		return errors.New("read rejected upstream response: " + providerreply.Failure(err))
	}
	upstreamResponse.Body = io.NopCloser(bytes.NewReader(body))
	var rejected httpJSONResponseWire
	_ = json.Unmarshal(body, &rejected)
	if limitOutcome, limited := responseObserver.reportProviderUsageLimit(rejected.Error); limited {
		log.Printf("%s: %s; Codex retries", subject, limitOutcome)
		return errUpstreamRejectedBeforeAdmission
	}
	if responseObserver.reportUnsupportedModel(rejected.Error) || responseObserver.reportUnsupportedModelDetail(rejected.Detail) {
		log.Printf("%s: OpenAI says this account does not offer %s; Codex retries, and the retry goes to an account that offers it or waits for one", subject, routedRequest.modelSlug)
		return errUpstreamRejectedBeforeAdmission
	}
	log.Printf("%s: OpenAI rejected %s with %s", subject, routedRequest.modelSlug, describeRefusal(upstreamResponse.Header, upstreamResponse.StatusCode, body))
	return nil
}

// describeRefusal is an error reply as the log shows it: its HTTP status and
// error codes, or that Cloudflare stopped the request before OpenAI; see
// providerreply.
func describeRefusal(header http.Header, status int, body []byte) string {
	if header.Get("Cf-Mitigated") != "" {
		described := fmt.Sprintf("HTTP %d: Cloudflare stopped the request before OpenAI", status)
		if mitigation := providerreply.Code(header.Get("Cf-Mitigated")); mitigation != "" {
			described += " (cf-mitigated: " + mitigation + ")"
		}
		return described
	}
	return providerreply.Refusal(status, body)
}
