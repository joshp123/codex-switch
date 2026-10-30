package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/clientcapability"
	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
	"github.com/joshp123/codex-switch/internal/usage"
	"github.com/klauspost/compress/zstd"
)

const (
	codexResponsesPath        = "/backend-api/codex/responses"
	codexCompactResponsesPath = "/backend-api/codex/responses/compact"
	taskIDHeader              = "Thread-ID"
	chatGPTAccountIDHeader    = "ChatGPT-Account-ID"
)

var errUpstreamRejectedBeforeAdmission = errors.New("upstream account rejected work before admission")

type relayRequestContextKey struct{}
type requestTrailerSourceContextKey struct{}

type routedRelayRequest struct {
	provider                accounts.Provider
	cachePrefixes           []string
	taskID                  store.TaskID
	client                  string
	workKind                store.WorkKind
	modelSlug               string
	responseStreamRequested bool
	routeDecision           routing.RouteDecision
	requestTrailerSource    http.Header
	responseObserver        *terminalResponseObserver
}

// RelayHandlerOptions keeps listener and signal ownership outside the transport package.
type RelayHandlerOptions struct {
	UpstreamOrigin       *url.URL
	ClaudeUpstreamOrigin *url.URL
	AccountObserver      ClaudeAccountObserver
	CacheAffinity        *store.StoreDatabase
	ClientCapability     string
	TaskRouter           TaskRouter
	CompletionLedger     CompletionLedger
	RequestCounters      ModelRequestCounters
	HTTPTransport        http.RoundTripper
	OnFailure            func()
	OnModelRequest       func()
}

// TrafficRelayHandler stays alive with admitted streams so deployment does not cut off model work.
type TrafficRelayHandler struct {
	authenticatedHandler http.Handler
	taskRouter           TaskRouter
	completionLedger     CompletionLedger
	requestCounters      ModelRequestCounters
	upstreamOrigin       *url.URL
	claudeUpstreamOrigin *url.URL
	accountObserver      ClaudeAccountObserver
	cacheAffinity        *store.StoreDatabase
	upstreamTransport    http.RoundTripper
	reverseProxy         *httputil.ReverseProxy

	drainMutex         sync.Mutex
	activeRequestCount int
	drainStarted       bool
	drainComplete      chan struct{}
	openWebSockets     map[*terminalResponseObserver]struct{}
	closingWebSockets  bool
	stopping           chan struct{}
	onFailure          func()
	onModelRequest     func()
}

// CreateTrafficRelayHandler leaves listener ownership and signal handling with the command layer.
func CreateTrafficRelayHandler(options RelayHandlerOptions) (*TrafficRelayHandler, error) {
	if options.UpstreamOrigin == nil || options.UpstreamOrigin.Scheme == "" || options.UpstreamOrigin.Host == "" {
		return nil, errors.New("relay upstream origin is required")
	}
	if options.UpstreamOrigin.Scheme != "http" && options.UpstreamOrigin.Scheme != "https" {
		return nil, errors.New("relay upstream origin must use http or https")
	}
	if options.UpstreamOrigin.User != nil || options.UpstreamOrigin.Path != "" ||
		options.UpstreamOrigin.RawQuery != "" || options.UpstreamOrigin.Fragment != "" {
		return nil, errors.New("relay upstream origin must not include credentials, path, query, or fragment")
	}
	if options.ClaudeUpstreamOrigin == nil {
		options.ClaudeUpstreamOrigin = &url.URL{Scheme: "https", Host: "api.anthropic.com"}
	}
	origin := options.ClaudeUpstreamOrigin
	if (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, errors.New("Claude upstream origin must use http or https with no credentials, path, query, or fragment")
	}
	if options.TaskRouter == nil {
		return nil, errors.New("relay task router is required")
	}
	if options.CompletionLedger == nil {
		return nil, errors.New("relay completion ledger is required")
	}

	upstreamTransport := options.HTTPTransport
	if upstreamTransport == nil {
		upstreamTransport = NewUpstreamTransport()
	}
	relayHandler := &TrafficRelayHandler{
		taskRouter:           options.TaskRouter,
		completionLedger:     options.CompletionLedger,
		requestCounters:      options.RequestCounters,
		upstreamOrigin:       options.UpstreamOrigin,
		claudeUpstreamOrigin: options.ClaudeUpstreamOrigin,
		accountObserver:      options.AccountObserver,
		cacheAffinity:        options.CacheAffinity,
		upstreamTransport:    upstreamTransport,
		drainComplete:        make(chan struct{}),
		openWebSockets:       make(map[*terminalResponseObserver]struct{}),
		stopping:             make(chan struct{}),
		onFailure:            options.OnFailure,
		onModelRequest:       options.OnModelRequest,
	}
	relayHandler.reverseProxy = relayHandler.createBytePreservingReverseProxy()
	relayHandler.authenticatedHandler = clientcapability.RequirePathPrefix(options.ClientCapability, http.HandlerFunc(relayHandler.serveAuthenticatedTraffic))
	return relayHandler, nil
}

// ServeHTTP authenticates the private client path before any account credential is selected.
func (handler *TrafficRelayHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	requestWithTrailerSource := request.WithContext(context.WithValue(
		request.Context(), requestTrailerSourceContextKey{}, request.Trailer,
	))
	handler.authenticatedHandler.ServeHTTP(response, requestWithTrailerSource)
}

func (handler *TrafficRelayHandler) serveAuthenticatedTraffic(response http.ResponseWriter, request *http.Request) {
	handler.beginActiveRelayRequest()
	defer handler.finishActiveRelayRequest()

	provider := accounts.ProviderChatGPT
	if request.URL.Path == "/anthropic" || strings.HasPrefix(request.URL.Path, "/anthropic/") {
		provider = accounts.ProviderClaude
		request.URL.Path = strings.TrimPrefix(request.URL.Path, "/anthropic")
		request.URL.RawPath = strings.TrimPrefix(request.URL.RawPath, "/anthropic")
	}
	taskID := taskIdentityForRequest(request)
	if provider == accounts.ProviderClaude {
		taskID = claudeSessionForRequest(request, nil)
	}
	var parentTaskID store.TaskID
	var cachePrefixes []string
	workKind := workKindForRequest(request)
	if workKind == store.ModelTurnWorkKind && handler.onModelRequest != nil {
		handler.onModelRequest()
	}
	client := clientNameForRequest(request)
	var modelSlug, serviceTier, inferenceGeo string
	var responseStreamRequested bool
	var warmup bool
	if workKind == store.ModelTurnWorkKind && request.Body != nil {
		body, err := io.ReadAll(request.Body)
		request.Body.Close()
		if err != nil {
			log.Printf("%s: reading the model request from the client failed: %v", describeModelRequest(taskID, client, ""), err)
			http.Error(response, "read model request", http.StatusBadRequest)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		// A model POST is never idempotent. Without replay markers, Go uses
		// GetBody only when a reused HTTP/1 connection wrote zero bytes.
		request.GetBody = nil
		_, hasIdempotencyKey := request.Header["Idempotency-Key"]
		_, hasAlternateIdempotencyKey := request.Header["X-Idempotency-Key"]
		if !hasIdempotencyKey && !hasAlternateIdempotencyKey {
			request.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
		inspectedBody := body
		if strings.EqualFold(strings.TrimSpace(request.Header.Get("Content-Encoding")), "zstd") {
			decoder, err := zstd.NewReader(nil)
			if err == nil {
				inspectedBody, err = decoder.DecodeAll(body, nil)
				decoder.Close()
			}
			if err != nil {
				log.Printf("%s: the zstd-compressed model request could not be decoded: %v", describeModelRequest(taskID, client, ""), err)
				http.Error(response, "decode model request", http.StatusBadRequest)
				return
			}
		}
		var modelRequest struct {
			Model        string `json:"model"`
			ServiceTier  string `json:"service_tier"`
			Stream       bool   `json:"stream"`
			Generate     *bool  `json:"generate"`
			Speed        string `json:"speed"`
			InferenceGeo string `json:"inference_geo"`
		}
		if json.Unmarshal(inspectedBody, &modelRequest) == nil {
			modelSlug = modelRequest.Model
			serviceTier = modelRequest.ServiceTier
			if provider == accounts.ProviderClaude {
				serviceTier = modelRequest.Speed
				inferenceGeo = modelRequest.InferenceGeo
			}
			responseStreamRequested = modelRequest.Stream
			warmup = modelRequest.Generate != nil && !*modelRequest.Generate
		}
		if provider == accounts.ProviderClaude {
			cachePrefixes = claudeConversationPrefixes(inspectedBody)
			taskID = claudeSessionForRequest(request, cachePrefixes)
			if taskID != "" && parentTaskID == "" && handler.cacheAffinity != nil {
				parentTaskID, err = handler.cacheAffinity.ReadPromptCacheParent(taskID, cachePrefixes)
				if err != nil {
					log.Printf("read Claude cache affinity: %v", err)
				}
			}
		}
	}
	var routeDecision routing.RouteDecision
	var err error
	if provider == accounts.ProviderChatGPT && request.Method == http.MethodGet && request.URL.Path == "/backend-api/codex/models" {
		routeDecision, err = handler.taskRouter.RouteForModelCatalogue()
	} else {
		routeDecision, err = handler.taskRouter.RouteForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
	}
	if workKind == store.ModelTurnWorkKind && errors.Is(err, routing.ErrNoAvailableAccount) {
		waitStarted := time.Now()
		log.Printf("%s waits before sending %s to its provider: %v", describeModelRequest(taskID, client, ""), modelSlug, err)
		recheck := time.NewTicker(5 * time.Second)
		defer recheck.Stop()
		for errors.Is(err, routing.ErrNoAvailableAccount) {
			select {
			case <-request.Context().Done():
				log.Printf("%s stopped waiting for an account after %s: the client closed the request", describeModelRequest(taskID, client, ""), time.Since(waitStarted).Round(time.Second))
				return
			case <-handler.stopping:
				response.Header().Set("Connection", "close")
				response.Header().Set("Retry-After", "1")
				http.Error(response, "proxy updating; retry request", http.StatusServiceUnavailable)
				return
			case <-recheck.C:
				routeDecision, err = handler.taskRouter.RouteForProviderTask(taskID, workKind, modelSlug, provider, parentTaskID)
			}
		}
		if err == nil {
			log.Printf("%s stopped waiting after %s", describeModelRequest(taskID, client, routeDecision.DisplayName), time.Since(waitStarted).Round(time.Second))
		}
	}
	if err != nil {
		if request.Context().Err() == nil && !errors.Is(err, routing.ErrNoAvailableAccount) && handler.onFailure != nil {
			handler.onFailure()
		}
		log.Printf("%s could not be routed: %v", describeModelRequest(taskID, client, ""), err)
		http.Error(response, "codex-switch could not choose an account for this request; see the codex-switch proxy log", http.StatusServiceUnavailable)
		return
	}
	if routeDecision.Assignment != "" {
		log.Printf("%s: %s", describeModelRequest(taskID, client, ""), routeDecision.Assignment)
	}

	requestTrailerSource, _ := request.Context().Value(requestTrailerSourceContextKey{}).(http.Header)
	routedRequest := &routedRelayRequest{
		taskID: taskID, client: client, workKind: workKind, modelSlug: modelSlug, responseStreamRequested: responseStreamRequested, routeDecision: routeDecision,
		requestTrailerSource: requestTrailerSource, provider: provider, cachePrefixes: cachePrefixes,
	}
	forwardedRequest := request.Clone(context.WithValue(request.Context(), relayRequestContextKey{}, routedRequest))
	if workKind == store.ModelTurnWorkKind {
		observer := newTerminalResponseObserver(taskID, client, routeDecision.AccountName, handler.taskRouter, handler.completionLedger)
		observer.accountDisplayName = routeDecision.DisplayName
		if provider == accounts.ProviderClaude {
			observer.claudeUsage = usage.NewClaudeResponseUsage(modelSlug, serviceTier, inferenceGeo)
		}
		observer.modelSlug = modelSlug
		observer.serviceTier = serviceTier
		if provider == accounts.ProviderChatGPT {
			observer.forkSourceTaskID = forkSourceTaskForRequest(request)
		}
		observer.requestCounters = handler.requestCounters
		observer.onFailure = func() {
			if request.Context().Err() == nil && handler.onFailure != nil {
				handler.onFailure()
			}
		}
		observer.onModelRequest = handler.onModelRequest
		routedRequest.responseObserver = observer
		if request.Method == http.MethodPost {
			observer.startModelRequest(warmup)
			observer.admittedRequests = 1
		}
		defer observer.finishModelRequests()
		defer handler.forgetOpenWebSocket(observer)
	}
	handler.reverseProxy.ServeHTTP(response, forwardedRequest)
}

func (handler *TrafficRelayHandler) createBytePreservingReverseProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			routedRequest := proxyRequest.In.Context().Value(relayRequestContextKey{}).(*routedRelayRequest)
			origin := handler.upstreamOrigin
			if routedRequest.provider == accounts.ProviderClaude {
				origin = handler.claudeUpstreamOrigin
			}
			proxyRequest.SetURL(origin)
			proxyRequest.Out.Host = origin.Host
			proxyRequest.Out.Header.Del("Accept-Encoding")
			if proxyRequest.Out.Body != nil && len(proxyRequest.Out.Trailer) > 0 {
				proxyRequest.Out.Body = &requestTrailerBody{
					ReadCloser:  proxyRequest.Out.Body,
					source:      routedRequest.requestTrailerSource,
					destination: proxyRequest.Out.Trailer,
				}
			}
			proxyRequest.Out.Header.Set("Authorization", "Bearer "+routedRequest.routeDecision.Credentials.AccessToken)
			if routedRequest.provider == accounts.ProviderClaude {
				if routedRequest.workKind == store.ModelTurnWorkKind && routedRequest.responseStreamRequested {
					proxyRequest.Out.Header.Set("Accept-Encoding", "identity")
				}
				prepareClaudeHeaders(proxyRequest.Out.Header)
			} else {
				proxyRequest.Out.Header.Set(chatGPTAccountIDHeader, routedRequest.routeDecision.Credentials.AccountID)
			}
		},
		Transport: handler.upstreamTransport,
		ModifyResponse: func(upstreamResponse *http.Response) error {
			routedRequest := upstreamResponse.Request.Context().Value(relayRequestContextKey{}).(*routedRelayRequest)
			if routedRequest.provider == accounts.ProviderClaude {
				if handler.accountObserver != nil {
					handler.accountObserver.ObserveClaudeResponse(routedRequest.routeDecision.AccountName, routedRequest.modelSlug, upstreamResponse.Header, upstreamResponse.StatusCode)
				}
				if upstreamResponse.StatusCode >= http.StatusBadRequest {
					return handler.observeClaudeRejection(routedRequest, upstreamResponse)
				}
				if routedRequest.workKind == store.ModelTurnWorkKind && handler.cacheAffinity != nil && len(routedRequest.cachePrefixes) > 0 {
					if err := handler.cacheAffinity.RememberPromptCachePrefix(routedRequest.taskID, routedRequest.cachePrefixes[len(routedRequest.cachePrefixes)-1]); err != nil {
						log.Printf("remember Claude cache affinity: %v", err)
					}
				}
			}
			if upstreamResponse.StatusCode >= http.StatusBadRequest {
				return handler.observeRejectedResponse(routedRequest, upstreamResponse)
			}
			if routedRequest.workKind != store.ModelTurnWorkKind {
				return nil
			}
			responseObserver := routedRequest.responseObserver

			if upstreamResponse.StatusCode == http.StatusSwitchingProtocols {
				upgradedConnection, ok := upstreamResponse.Body.(io.ReadWriteCloser)
				if !ok {
					return errors.New("upstream WebSocket connection cannot be relayed")
				}
				observedConnection := createObservedWebSocketConnection(
					upgradedConnection,
					upstreamResponse.Header.Get("Sec-WebSocket-Extensions"),
					responseObserver.observeWebSocketServerEvent,
				)
				observedConnection.observeClientMessages(responseObserver.routeClientMessage)
				observedConnection.reportReadError = responseObserver.logReadError
				upstreamResponse.Body = observedConnection
				handler.trackOpenWebSocket(responseObserver, func() { observedConnection.Close() })
				return nil
			}

			contentType := strings.ToLower(upstreamResponse.Header.Get("Content-Type"))
			if strings.HasPrefix(contentType, "text/event-stream") || (routedRequest.responseStreamRequested && upstreamResponse.StatusCode == http.StatusOK) {
				observedBody := createObservedResponseBody(
					upstreamResponse.Body,
					createServerSentEventObserver(responseObserver.observeStructuredEvent, responseObserver.shouldObserveStreamEvent),
				)
				observedBody.reportReadError = responseObserver.logReadError
				upstreamResponse.Body = observedBody
				return nil
			}
			if strings.HasPrefix(contentType, "application/json") {
				observedBody := createObservedResponseBody(
					upstreamResponse.Body,
					createJSONResponseObserver(responseObserver.observeHTTPJSONBody),
				)
				observedBody.reportReadError = responseObserver.logReadError
				upstreamResponse.Body = observedBody
			}
			return nil
		},
		ErrorHandler: func(response http.ResponseWriter, request *http.Request, err error) {
			routedRequest := request.Context().Value(relayRequestContextKey{}).(*routedRelayRequest)
			subject := describeModelRequest(routedRequest.taskID, routedRequest.client, routedRequest.routeDecision.DisplayName)
			switch {
			case errors.Is(err, errUpstreamRejectedBeforeAdmission):
				// Logged when the rejection was observed.
			case request.Context().Err() != nil:
				// A model POST counts as interrupted; a cancelled metadata read or
				// WebSocket handshake carries no model work.
				if routedRequest.workKind == store.ModelTurnWorkKind && request.Method == http.MethodPost {
					log.Printf("%s: the client closed the %s request before the provider answered", subject, routedRequest.modelSlug)
				}
			default:
				if isUpstreamProtocolFailure(err) && handler.onFailure != nil {
					handler.onFailure()
				}
				log.Printf("%s: the upstream request failed: %v; the proxy did not resend this attempt", subject, err)
			}
			http.Error(response, "upstream service unavailable", http.StatusBadGateway)
		},
		FlushInterval: -1,
	}
}

// ReverseProxy clones request trailers before the inbound body has populated their final values.
type requestTrailerBody struct {
	io.ReadCloser
	source      http.Header
	destination http.Header
}

func (body *requestTrailerBody) Read(buffer []byte) (int, error) {
	bytesRead, err := body.ReadCloser.Read(buffer)
	if errors.Is(err, io.EOF) {
		for headerName, values := range body.source {
			body.destination[headerName] = append([]string(nil), values...)
		}
	}
	return bytesRead, err
}

func workKindForRequest(request *http.Request) store.WorkKind {
	if request.URL.Path == codexResponsesPath || request.URL.Path == codexCompactResponsesPath || (request.Method == http.MethodPost && request.URL.Path == "/v1/messages") {
		return store.ModelTurnWorkKind
	}
	return store.TaskMetadataWorkKind
}

func (handler *TrafficRelayHandler) beginActiveRelayRequest() {
	handler.drainMutex.Lock()
	handler.activeRequestCount++
	handler.drainMutex.Unlock()
}

func (handler *TrafficRelayHandler) finishActiveRelayRequest() {
	handler.drainMutex.Lock()
	handler.activeRequestCount--
	if handler.drainStarted && handler.activeRequestCount == 0 {
		select {
		case <-handler.drainComplete:
		default:
			close(handler.drainComplete)
		}
	}
	handler.drainMutex.Unlock()
}

func (handler *TrafficRelayHandler) closeIdleUpstreamConnections() {
	if idleConnectionCloser, ok := handler.upstreamTransport.(interface{ CloseIdleConnections() }); ok {
		idleConnectionCloser.CloseIdleConnections()
	}
}

var _ http.Handler = (*TrafficRelayHandler)(nil)

// Protocol errors identify a response the relay could not interpret, unlike a
// network failure or an HTTP error returned by the provider.
func isUpstreamProtocolFailure(err error) bool {
	return strings.Contains(err.Error(), "malformed HTTP ") || strings.Contains(err.Error(), "server gave HTTP response to HTTPS client")
}
