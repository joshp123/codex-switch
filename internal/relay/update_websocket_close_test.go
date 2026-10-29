package relay

import (
	"testing"
	"time"
)

// An update must let a streaming response finish, then close its WebSocket,
// so the client's next request reaches the new process and the old one exits.
func TestUpdateClosesWebSocketOnlyAfterItsResponseEnds(t *testing.T) {
	handler := &TrafficRelayHandler{openWebSockets: map[*terminalResponseObserver]struct{}{}, drainComplete: make(chan struct{}), stopping: make(chan struct{})}

	streaming := newTerminalResponseObserver("", "test-client", "account-1", nil, &recordedHTTPCompletion{})
	streamingClosed := make(chan time.Time, 1)
	handler.trackOpenWebSocket(streaming, func() { streamingClosed <- time.Now() })
	if streaming.routeClientMessage([]byte(`{"type":"response.create","model":"luna"}`)) {
		t.Fatal("a request before the update was not forwarded")
	}
	streaming.observeStructuredEvent([]byte(`{"type":"response.created","response":{"id":"reply-1"}}`))

	idle := newTerminalResponseObserver("", "test-client", "account-2", nil, &recordedHTTPCompletion{})
	idleClosed := make(chan time.Time, 1)
	handler.trackOpenWebSocket(idle, func() { idleClosed <- time.Now() })
	idle.idleSince = time.Now().Add(-updateWebSocketQuietPeriod)

	handler.CloseWebSocketsOnceIdle()
	select {
	case <-idleClosed:
	case <-time.After(time.Second):
		t.Fatal("an idle WebSocket stayed open during the update")
	}
	select {
	case <-streamingClosed:
		t.Fatal("the update closed a WebSocket while its response was streaming")
	case <-time.After(2 * updateWebSocketQuietPeriod):
	}

	completedAt := time.Now()
	streaming.observeStructuredEvent([]byte(`{"type":"response.completed","response":{"id":"reply-1","model":"luna","usage":{"input_tokens":10,"output_tokens":2}}}`))
	select {
	case closedAt := <-streamingClosed:
		if closedAt.Sub(completedAt) < updateWebSocketQuietPeriod {
			t.Fatalf("closed %s after the response completed, before the quiet period", closedAt.Sub(completedAt))
		}
	case <-time.After(2 * updateWebSocketQuietPeriod):
		t.Fatal("the WebSocket stayed open after its response completed")
	}
	if !idle.routeClientMessage([]byte(`{"type":"response.create","model":"luna"}`)) {
		t.Fatal("a request on a closing WebSocket was forwarded to the old process")
	}
}
