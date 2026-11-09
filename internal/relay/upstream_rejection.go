package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/joshp123/codex-switch/internal/store"
)

// observeRejectedResponse logs every request OpenAI answers with HTTP 400 or
// above, with the reason OpenAI gave. A model request rejected before OpenAI
// started work because its account is out of allowance, or does not offer the
// model, moves its task and becomes a retryable error for Codex; the proxy never
// resends the request itself. Every other rejection reaches the client unchanged.
func (handler *TrafficRelayHandler) observeRejectedResponse(routedRequest *routedRelayRequest, upstreamResponse *http.Response) error {
	subject := describeModelRequest(routedRequest.taskID, routedRequest.client, routedRequest.routeDecision.DisplayName)
	if routedRequest.workKind != store.ModelTurnWorkKind {
		// Read only the start of the body for the reason, then hand the client
		// every byte unchanged.
		head, _ := io.ReadAll(io.LimitReader(upstreamResponse.Body, 4096))
		upstreamResponse.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), upstreamResponse.Body), upstreamResponse.Body}
		log.Printf("%s: OpenAI answered %s %s with HTTP %d%s", subject, upstreamResponse.Request.Method, upstreamResponse.Request.URL.Path, upstreamResponse.StatusCode, describeRejection(upstreamResponse.Header, head))
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
		log.Printf("%s: OpenAI rejected %s with HTTP %d and its reason could not be read: %v", subject, routedRequest.modelSlug, upstreamResponse.StatusCode, err)
		return fmt.Errorf("read rejected upstream response: %w", err)
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
	log.Printf("%s: OpenAI rejected %s with HTTP %d%s", subject, routedRequest.modelSlug, upstreamResponse.StatusCode, describeRejection(upstreamResponse.Header, body))
	return nil
}

// describeRejection quotes why a request was refused: Cloudflare's challenge
// when Cloudflare stopped it before OpenAI, OpenAI's API error when the body is
// one, otherwise the page title or the start of the text, on one line,
// shortened to 300 bytes. body may be only the start of the response.
func describeRejection(header http.Header, body []byte) string {
	return shortened(describeRejectionText(header, body))
}

// shortened keeps the first 300 bytes of a provider's reason. The proxy log
// removes a credential the cut splits; see heldcredentials.
func shortened(reason string) string {
	if len(reason) > 300 {
		reason = reason[:300] + "..."
	}
	return reason
}

func describeRejectionText(header http.Header, body []byte) string {
	if mitigation := header.Get("Cf-Mitigated"); mitigation != "" {
		return fmt.Sprintf(": Cloudflare stopped the request before OpenAI (cf-mitigated: %s, cf-ray: %s)", mitigation, header.Get("Cf-Ray"))
	}
	var rejected httpJSONResponseWire
	if json.Unmarshal(body, &rejected) == nil {
		return describeProviderReason(rejected)
	}
	text := string(body)
	if !strings.Contains(strings.ToLower(header.Get("Content-Type")), "html") {
		return describeProviderReason(httpJSONResponseWire{Detail: text})
	}
	if start := strings.Index(strings.ToLower(text), "<title>"); start >= 0 {
		if end := strings.Index(strings.ToLower(text[start:]), "</title>"); end >= 0 {
			return describeProviderReason(httpJSONResponseWire{Detail: "an HTML page titled " + text[start+len("<title>"):start+end]})
		}
	}
	return ": an HTML page instead of an API error"
}

// describeProviderReason quotes the error OpenAI returned, on one line, in
// full; see shortened.
func describeProviderReason(rejected httpJSONResponseWire) string {
	reason := rejected.Detail
	if rejected.Error != nil {
		reason = strings.TrimSpace(rejected.Error.Type + " " + rejected.Error.Message)
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		return ""
	}
	return ": " + reason
}
