package relay

import (
	"log"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

type observedModelRequest struct {
	admittedAt time.Time
	warmup     bool
	responseID string
	settled    bool
}

func (observer *terminalResponseObserver) startModelRequest(warmup bool) {
	allSettled := true
	for _, request := range observer.requests {
		allSettled = allSettled && request.settled
	}
	if allSettled {
		observer.requests = observer.requests[:0]
	}
	request := &observedModelRequest{admittedAt: time.Now().UTC(), warmup: warmup}
	observer.requests = append(observer.requests, request)
	observer.recordRequestOutcome(request, store.ModelRequestStarted)
}

func (observer *terminalResponseObserver) recordRequestOutcome(request *observedModelRequest, outcome store.ModelRequestOutcome) {
	if request.warmup || observer.requestCounters == nil {
		return
	}
	if err := observer.requestCounters.RecordModelRequestOutcome(observer.accountKey, request.admittedAt, outcome); err != nil {
		log.Printf("%s: could not record the request count: %v", observer.describe(), err)
	}
}

func (observer *terminalResponseObserver) requestForResponse(responseID string) *observedModelRequest {
	if responseID != "" {
		for _, request := range observer.requests {
			if request.responseID == responseID {
				return request
			}
		}
	}
	var unmatched *observedModelRequest
	for _, request := range observer.requests {
		if !request.settled && request.responseID == "" {
			if unmatched != nil {
				return nil
			}
			unmatched = request
		}
	}
	if unmatched != nil && responseID != "" {
		unmatched.responseID = responseID
	}
	if unmatched != nil {
		return unmatched
	}
	if responseID == "" {
		for _, request := range observer.requests {
			if !request.settled {
				if unmatched != nil {
					return nil
				}
				unmatched = request
			}
		}
	}
	return unmatched
}

func (observer *terminalResponseObserver) settleModelRequest(responseID string, outcome store.ModelRequestOutcome) {
	request := observer.requestForResponse(responseID)
	if request == nil || request.settled {
		return
	}
	request.settled = true
	observer.recordRequestOutcome(request, outcome)
}

func (observer *terminalResponseObserver) finishModelRequests() {
	observer.admissionMutex.Lock()
	defer observer.admissionMutex.Unlock()
	for _, request := range observer.requests {
		if request.settled {
			continue
		}
		request.settled = true
		observer.recordRequestOutcome(request, store.ModelRequestInterrupted)
	}
}
