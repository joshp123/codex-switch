package relay

import (
	"context"
	"log"
	"time"
)

// updateWebSocketQuietPeriod is how long a WebSocket must have had no response
// in progress before an update closes it. Pi's WebSocket (Node's undici) can
// report a close before the compressed message that arrived just before it;
// a close straight after response.completed made Pi request the same response
// again. Codex is unaffected either way.
const updateWebSocketQuietPeriod = time.Second

// CloseWebSocketsOnceIdle starts an update. Responses in progress finish on
// this process. Each WebSocket, including any upgraded later, closes once it
// has had no response in progress for the quiet period; a request arriving
// before then closes it unforwarded. Codex and Pi then send their next
// request on a new connection, which reaches the new process.
func (handler *TrafficRelayHandler) CloseWebSocketsOnceIdle() {
	handler.drainMutex.Lock()
	if !handler.closingWebSockets {
		close(handler.stopping)
	}
	handler.closingWebSockets = true
	openWebSockets := make([]*terminalResponseObserver, 0, len(handler.openWebSockets))
	for observer := range handler.openWebSockets {
		openWebSockets = append(openWebSockets, observer)
	}
	handler.drainMutex.Unlock()
	for _, observer := range openWebSockets {
		observer.closeForUpdateOnceIdle()
	}
}

// BeginRelayDrain closes idle upstream keep-alives after the command layer stops admission.
func (handler *TrafficRelayHandler) BeginRelayDrain() {
	handler.drainMutex.Lock()
	if !handler.drainStarted {
		handler.drainStarted = true
		if handler.activeRequestCount == 0 {
			close(handler.drainComplete)
		}
	}
	handler.drainMutex.Unlock()
	handler.closeIdleUpstreamConnections()
}

// WaitForRelayDrain returns once every admitted request has finished.
func (handler *TrafficRelayHandler) WaitForRelayDrain(ctx context.Context) error {
	select {
	case <-handler.drainComplete:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (handler *TrafficRelayHandler) trackOpenWebSocket(observer *terminalResponseObserver, closeWebSocket func()) {
	observer.admissionMutex.Lock()
	observer.closeWebSocket = closeWebSocket
	observer.idleSince = time.Now()
	observer.admissionMutex.Unlock()
	handler.drainMutex.Lock()
	handler.openWebSockets[observer] = struct{}{}
	closing := handler.closingWebSockets
	handler.drainMutex.Unlock()
	if closing {
		observer.closeForUpdateOnceIdle()
	}
}

func (handler *TrafficRelayHandler) forgetOpenWebSocket(observer *terminalResponseObserver) {
	handler.drainMutex.Lock()
	delete(handler.openWebSockets, observer)
	handler.drainMutex.Unlock()
	observer.admissionMutex.Lock()
	observer.webSocketClosed = true
	observer.admissionMutex.Unlock()
}

func (observer *terminalResponseObserver) closeForUpdateOnceIdle() {
	observer.admissionMutex.Lock()
	defer observer.admissionMutex.Unlock()
	observer.closingForUpdate = true
	observer.scheduleUpdateCloseIfIdle()
}

// noteResponseEnded runs with admissionMutex held after a terminal response event.
func (observer *terminalResponseObserver) noteResponseEnded() {
	if observer.activeResponses == 0 && observer.admittedRequests == 0 {
		observer.idleSince = time.Now()
		observer.scheduleUpdateCloseIfIdle()
	}
}

// scheduleUpdateCloseIfIdle runs with admissionMutex held. A closing socket
// that is idle stays idle: its next request closes it unforwarded.
func (observer *terminalResponseObserver) scheduleUpdateCloseIfIdle() {
	if !observer.closingForUpdate || observer.updateCloseScheduled ||
		observer.activeResponses > 0 || observer.admittedRequests > 0 {
		return
	}
	observer.updateCloseScheduled = true
	time.AfterFunc(time.Until(observer.idleSince.Add(updateWebSocketQuietPeriod)), observer.closeForUpdate)
}

func (observer *terminalResponseObserver) closeForUpdate() {
	observer.admissionMutex.Lock()
	alreadyClosed := observer.webSocketClosed
	observer.webSocketClosed = true
	observer.admissionMutex.Unlock()
	if alreadyClosed {
		return
	}
	log.Printf("%s: the WebSocket closes for the update with no response in progress; the next request opens a new connection", observer.describe())
	observer.closeWebSocket()
}
