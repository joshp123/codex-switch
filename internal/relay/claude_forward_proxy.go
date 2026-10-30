package relay

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A retiring proxy gives every connection doing no work this long before it
// closes it. Claude Code holds event streams open for as long as a session
// lives and reopens them, so only a model reply outlasts this on Anthropic's
// connections. A tunnel is opaque, so it counts as working while bytes move.
const claudeConnectionDrainGrace = 60 * time.Second

// ClaudeForwardProxy handles CONNECT on the same listener as the private API.
// Only Anthropic model POSTs enter the account pool; all other traffic keeps
// the client's credentials. Tunnel lifetime is part of the relay's drain.
type ClaudeForwardProxy struct {
	relay          *TrafficRelayHandler
	next           http.Handler
	certificate    tls.Certificate
	proxyAuth      string
	capabilityPath string
	passthrough    *httputil.ReverseProxy
	mutex          sync.Mutex
	connections    map[*claudeProxyConnection]struct{}
	drainDeadline  time.Time
}

type claudeProxyConnection struct {
	client          net.Conn
	clientAddress   string
	upstream        net.Conn
	cancelDial      context.CancelFunc
	host            string
	server          *http.Server
	modelInProgress bool
	lastRequest     string
	closing         bool
	lastTraffic     atomic.Int64
}

func CreateClaudeForwardProxy(dataDirectory, capability string, trafficRelay *TrafficRelayHandler, next http.Handler) (*ClaudeForwardProxy, error) {
	certificate, err := CreateClaudeProxyCertificate(dataDirectory)
	if err != nil {
		return nil, err
	}
	passthroughTransport := trafficRelay.upstreamTransport
	if transport, ok := passthroughTransport.(*upstreamTransport); ok {
		copy := transport.transport.Clone()
		copy.DisableCompression = true
		passthroughTransport = &upstreamTransport{transport: copy}
	} else if transport, ok := passthroughTransport.(*http.Transport); ok {
		copy := transport.Clone()
		copy.DisableCompression = true
		passthroughTransport = copy
	}
	return &ClaudeForwardProxy{
		relay: trafficRelay, next: next, certificate: certificate,
		proxyAuth:      "Basic " + base64.StdEncoding.EncodeToString([]byte("codex-switch:"+capability)),
		capabilityPath: "/client/" + capability + "/anthropic",
		connections:    make(map[*claudeProxyConnection]struct{}),
		passthrough: &httputil.ReverseProxy{
			Rewrite: func(request *httputil.ProxyRequest) {
				request.SetURL(trafficRelay.claudeUpstreamOrigin)
				request.Out.Host = trafficRelay.claudeUpstreamOrigin.Host
				request.Out.Header.Del("Proxy-Authorization")
				if request.Out.Body != nil && len(request.Out.Trailer) > 0 {
					request.Out.Body = &requestTrailerBody{ReadCloser: request.Out.Body, source: request.In.Trailer, destination: request.Out.Trailer}
				}
			},
			Transport: passthroughTransport, FlushInterval: -1,
			ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
				if request.Context().Err() == nil {
					if isUpstreamProtocolFailure(err) && trafficRelay.onFailure != nil {
						trafficRelay.onFailure()
					}
					log.Printf("Claude account request %s %s failed: %v; the proxy did not resend this attempt", request.Method, request.URL.Path, err)
				}
				http.Error(writer, "upstream service unavailable", http.StatusBadGateway)
			},
		},
	}, nil
}

func (proxy *ClaudeForwardProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodConnect {
		proxy.next.ServeHTTP(writer, request)
		return
	}
	if subtle.ConstantTimeCompare([]byte(request.Header.Get("Proxy-Authorization")), []byte(proxy.proxyAuth)) != 1 {
		writer.Header().Set("Proxy-Authenticate", `Basic realm="codex-switch"`)
		http.Error(writer, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if _, _, err := net.SplitHostPort(request.Host); err != nil {
		http.Error(writer, "CONNECT requires a host and port", http.StatusBadRequest)
		return
	}
	proxy.relay.beginActiveRelayRequest()
	defer proxy.relay.finishActiveRelayRequest()
	dialContext, cancelDial := context.WithCancel(request.Context())
	defer cancelDial()
	tracked := &claudeProxyConnection{clientAddress: request.RemoteAddr, host: request.Host, cancelDial: cancelDial, lastRequest: "TLS handshake"}
	tracked.lastTraffic.Store(time.Now().UnixNano())
	proxy.mutex.Lock()
	proxy.connections[tracked] = struct{}{}
	if !proxy.drainDeadline.IsZero() && !time.Now().Before(proxy.drainDeadline) {
		proxy.closeConnectionForUpdate(tracked)
	}
	proxy.mutex.Unlock()
	defer func() {
		proxy.mutex.Lock()
		delete(proxy.connections, tracked)
		proxy.mutex.Unlock()
	}()

	var upstream net.Conn
	if request.Host != "api.anthropic.com:443" {
		var err error
		upstream, err = (&net.Dialer{}).DialContext(dialContext, "tcp", request.Host)
		if err != nil {
			if dialContext.Err() == nil {
				log.Printf("Claude tunnel to %s failed: %v", request.Host, err)
			}
			http.Error(writer, "upstream service unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
	}
	connection, buffered, err := writer.(http.Hijacker).Hijack()
	if err != nil {
		log.Printf("open Claude tunnel: %v", err)
		return
	}
	defer connection.Close()
	proxy.mutex.Lock()
	tracked.client, tracked.upstream = connection, upstream
	if tracked.closing {
		proxy.closeConnectionForUpdate(tracked)
	}
	proxy.mutex.Unlock()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	client := &bufferedTunnelConnection{Conn: connection, reader: buffered.Reader}
	if upstream != nil {
		finished := make(chan struct{})
		go func() {
			if _, err := io.Copy(upstream, tunnelTrafficReader{Reader: client, connection: tracked}); err != nil {
				_ = upstream.Close()
			} else if tcp, ok := upstream.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
			close(finished)
		}()
		if _, err := io.Copy(client, tunnelTrafficReader{Reader: upstream, connection: tracked}); err != nil {
			_ = upstream.Close()
			_ = client.Close()
		} else if tcp, ok := connection.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		<-finished
		return
	}
	proxy.serveDecryptedConnection(client, tracked)
}

func (proxy *ClaudeForwardProxy) serveDecryptedConnection(connection net.Conn, tracked *claudeProxyConnection) {
	listener := &singleTunnelListener{connection: connection, closed: make(chan struct{}), stopAccept: make(chan struct{})}
	listener.connection = tls.Server(&closeTrackedTunnelConnection{Conn: connection, listener: listener}, &tls.Config{
		Certificates: []tls.Certificate{proxy.certificate}, MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	server := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			proxy.serveAnthropicRequest(writer, request, tracked)
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			// These states follow the final HTTP response flush. A handler defer
			// would expose the last buffered model bytes to the grace timer.
			if state == http.StateIdle || state == http.StateClosed {
				proxy.mutex.Lock()
				tracked.modelInProgress = false
				proxy.mutex.Unlock()
			}
		},
	}
	proxy.mutex.Lock()
	tracked.server = server
	draining := !proxy.drainDeadline.IsZero()
	proxy.mutex.Unlock()
	if draining {
		go server.Shutdown(context.Background())
	}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed && err != net.ErrClosed {
		log.Printf("serve Claude TLS connection: %v", err)
	}
	// Serve returns on listener close during Shutdown, before admitted HTTP
	// work finishes. The connection closes only after that work (or an upgrade).
	<-listener.closed
}

func (proxy *ClaudeForwardProxy) serveAnthropicRequest(writer http.ResponseWriter, request *http.Request, tracked *claudeProxyConnection) {
	modelRequest := request.Method == http.MethodPost && request.URL.Path == "/v1/messages"
	proxy.mutex.Lock()
	if tracked.closing {
		proxy.mutex.Unlock()
		return
	}
	tracked.modelInProgress = modelRequest
	tracked.lastRequest = request.Method + " " + request.URL.Path
	proxy.mutex.Unlock()
	if modelRequest {
		forwarded := request.Clone(request.Context())
		forwarded.URL.Path = proxy.capabilityPath + request.URL.Path
		if request.URL.RawPath != "" {
			forwarded.URL.RawPath = proxy.capabilityPath + request.URL.RawPath
		}
		proxy.relay.ServeHTTP(writer, forwarded)
		return
	}
	proxy.passthrough.ServeHTTP(writer, request)
}

// DrainConnections gives connections doing no work one grace period. HTTP
// shutdown closes idle connections now and model connections after their reply.
func (proxy *ClaudeForwardProxy) DrainConnections() {
	proxy.drainConnectionsWithin(claudeConnectionDrainGrace)
}

func (proxy *ClaudeForwardProxy) drainConnectionsWithin(grace time.Duration) {
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	if !proxy.drainDeadline.IsZero() {
		return
	}
	proxy.drainDeadline = time.Now().Add(grace)
	models, otherAnthropic := 0, 0
	tunnels := make(map[string]int)
	for connection := range proxy.connections {
		if connection.host != "api.anthropic.com:443" {
			tunnels[connection.host]++
		} else if connection.modelInProgress {
			models++
		} else {
			otherAnthropic++
		}
		if connection.server != nil {
			go connection.server.Shutdown(context.Background())
		}
	}
	var tunnelSummary []string
	for host, count := range tunnels {
		tunnelSummary = append(tunnelSummary, fmt.Sprintf("%s (%d)", host, count))
	}
	sort.Strings(tunnelSummary)
	if len(tunnelSummary) == 0 {
		tunnelSummary = []string{"none"}
	}
	log.Printf("Claude connections stopping: model requests in progress %d; other Anthropic connections %d; tunnels %s; connections doing no work close after %s", models, otherAnthropic, strings.Join(tunnelSummary, ", "), grace)
	go func() {
		time.Sleep(grace)
		for now := range time.Tick(grace / 10) {
			proxy.closeConnectionsDoingNoWork(now.Add(-grace))
		}
	}()
}

func (proxy *ClaudeForwardProxy) closeConnectionsDoingNoWork(quietSince time.Time) {
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	for connection := range proxy.connections {
		transferring := connection.host != "api.anthropic.com:443" && connection.lastTraffic.Load() > quietSince.UnixNano()
		if !connection.modelInProgress && !transferring {
			proxy.closeConnectionForUpdate(connection)
		}
	}
}

// The caller holds mutex, so model admission cannot race a grace-period close.
func (proxy *ClaudeForwardProxy) closeConnectionForUpdate(connection *claudeProxyConnection) {
	if !connection.closing {
		connection.closing = true
		if connection.host == "api.anthropic.com:443" {
			log.Printf("Claude Anthropic connection from %s (%s) closes for the update after the grace period with no model request in progress; the client reconnects", connection.clientAddress, connection.lastRequest)
		} else {
			log.Printf("Claude tunnel to %s from %s closes for the update after the grace period with no traffic; the client reconnects on its next request", connection.host, connection.clientAddress)
		}
	}
	connection.cancelDial()
	if connection.client != nil {
		_ = connection.client.Close()
	}
	if connection.upstream != nil {
		_ = connection.upstream.Close()
	}
}

type tunnelTrafficReader struct {
	io.Reader
	connection *claudeProxyConnection
}

func (reader tunnelTrafficReader) Read(buffer []byte) (int, error) {
	count, err := reader.Reader.Read(buffer)
	if count > 0 {
		reader.connection.lastTraffic.Store(time.Now().UnixNano())
	}
	return count, err
}

type bufferedTunnelConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (connection *bufferedTunnelConnection) Read(buffer []byte) (int, error) {
	return connection.reader.Read(buffer)
}

type singleTunnelListener struct {
	connection net.Conn
	mutex      sync.Mutex
	accepted   bool
	closed     chan struct{}
	stopAccept chan struct{}
	closeOnce  sync.Once
}

func (listener *singleTunnelListener) Accept() (net.Conn, error) {
	listener.mutex.Lock()
	select {
	case <-listener.stopAccept:
		listener.mutex.Unlock()
		return nil, net.ErrClosed
	default:
	}
	if !listener.accepted {
		listener.accepted = true
		listener.mutex.Unlock()
		return listener.connection, nil
	}
	listener.mutex.Unlock()
	select {
	case <-listener.closed:
	case <-listener.stopAccept:
	}
	return nil, net.ErrClosed
}

func (listener *singleTunnelListener) Close() error {
	listener.mutex.Lock()
	defer listener.mutex.Unlock()
	listener.closeOnce.Do(func() {
		close(listener.stopAccept)
		if !listener.accepted {
			_ = listener.connection.Close()
		}
	})
	return nil
}

func (listener *singleTunnelListener) Addr() net.Addr { return listener.connection.LocalAddr() }

type closeTrackedTunnelConnection struct {
	net.Conn
	listener *singleTunnelListener
	once     sync.Once
}

func (connection *closeTrackedTunnelConnection) Close() error {
	err := connection.Conn.Close()
	connection.once.Do(func() { close(connection.listener.closed) })
	return err
}

// ClaudeProxyURL carries the same private capability as the API entry point.
func ClaudeProxyURL(address, capability string) string {
	return fmt.Sprintf("http://codex-switch:%s@%s", capability, address)
}
