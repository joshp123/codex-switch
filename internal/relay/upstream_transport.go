package relay

import (
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/joshp123/codex-switch/internal/providerreply"
)

// NewUpstreamTransport is the one way this process reaches providers: account
// reads, the deployment provider check and client traffic all use it, so the
// check proves the path that serves traffic.
func NewUpstreamTransport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// One active request per connection. Go's HTTP/2 transport also retries
	// PROTOCOL_ERROR, which does not prove that model work was refused.
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	// The default TLS settings offer h2 in ALPN whatever Protocols says; a
	// provider that accepts it then sends frames this transport cannot read.
	transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	return &upstreamTransport{transport: transport}
}

type upstreamTransport struct {
	transport *http.Transport
}

func (transport *upstreamTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var gotConnection atomic.Bool
	var connectionAttempts atomic.Int64
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { gotConnection.Store(true) },
		GetConn: func(string) {
			if connectionAttempts.Add(1) > 1 && gotConnection.Load() {
				log.Printf("%s: the upstream connection failed; the proxy resends the request", upstreamRequestDescription(request))
			}
		},
	}
	forwarded := request.Clone(httptrace.WithClientTrace(request.Context(), trace))
	// A failed dial or TLS handshake closes Body even though it was never read.
	// Keep an unbuffered passthrough body open until a connection is acquired.
	if request.Body != nil && request.GetBody == nil {
		forwarded.Body = &unconnectedRequestBody{ReadCloser: request.Body, gotConnection: &gotConnection}
	}
	response, err := transport.transport.RoundTrip(forwarded)
	if err != nil && !gotConnection.Load() && request.Context().Err() == nil && isConnectionFailure(err) {
		connectionError := err
		if request.GetBody != nil {
			forwarded.Body, err = request.GetBody()
			if err != nil {
				return nil, err
			}
		}
		log.Printf("%s: opening the upstream connection failed: %s; the proxy resends the request", upstreamRequestDescription(request), providerreply.Failure(connectionError))
		response, err = transport.transport.RoundTrip(forwarded)
	}
	if err != nil && !gotConnection.Load() && request.Body != nil {
		request.Body.Close()
	}
	return response, err
}

func (transport *upstreamTransport) CloseIdleConnections() {
	transport.transport.CloseIdleConnections()
}

func isConnectionFailure(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

type unconnectedRequestBody struct {
	io.ReadCloser
	gotConnection *atomic.Bool
}

func (body *unconnectedRequestBody) Close() error {
	if !body.gotConnection.Load() {
		return nil
	}
	return body.ReadCloser.Close()
}

func upstreamRequestDescription(request *http.Request) string {
	if routed, ok := request.Context().Value(relayRequestContextKey{}).(*routedRelayRequest); ok {
		return describeModelRequest(routed.taskID, routed.client, routed.routeDecision.DisplayName)
	}
	return "account request " + request.Method + " " + request.URL.Host + request.URL.Path
}
