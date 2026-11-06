package relay

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

func TestWebSocketFragmentedMessageAllowsPingBeforeContinuation(t *testing.T) {
	upstream, downstream := net.Pipe()
	t.Cleanup(func() { upstream.Close(); downstream.Close() })
	upstream.SetDeadline(time.Now().Add(3 * time.Second))
	downstream.SetDeadline(time.Now().Add(3 * time.Second))
	first := reconnectTestFrame(0x01, []byte(`{"type":"response.`))
	last := reconnectTestFrame(0x80, []byte(`created"}`))
	ping := reconnectTestFrame(0x89, []byte("alive"))
	pong := reconnectTestFrame(0x8a, []byte("alive"))
	serverResult := make(chan error, 1)
	go func() {
		defer upstream.Close()
		if _, err := upstream.Write(append(first, ping...)); err != nil {
			serverResult <- err
			return
		}
		receivedPong := make([]byte, len(pong))
		if _, err := io.ReadFull(upstream, receivedPong); err != nil {
			serverResult <- err
			return
		}
		if !bytes.Equal(receivedPong, pong) {
			serverResult <- errors.New("upstream received different pong bytes")
			return
		}
		_, err := upstream.Write(last)
		serverResult <- err
	}()
	var observedMessage []byte
	connection := createObservedWebSocketConnection(downstream, "", func(message []byte) bool {
		observedMessage = append([]byte(nil), message...)
		return false
	})
	receivedPing := make([]byte, len(ping))
	if _, err := io.ReadFull(connection, receivedPing); err != nil {
		t.Fatalf("ping was not available before continuation: %v", err)
	}
	if !bytes.Equal(receivedPing, ping) {
		t.Fatalf("ping bytes changed: %x", receivedPing)
	}
	if _, err := connection.Write(pong); err != nil {
		t.Fatal(err)
	}
	receivedData, err := io.ReadAll(connection)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(receivedData, append(first, last...)) {
		t.Fatalf("fragment bytes changed: %x", receivedData)
	}
	if string(observedMessage) != `{"type":"response.created"}` {
		t.Fatalf("observed message = %q", observedMessage)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketUsageLimitReconnectOnlyBeforeResponseCreated(t *testing.T) {
	created := reconnectTestFrame(0x81, []byte(`{"type":"response.created","response":{"id":"response-1"}}`))
	limit := reconnectTestFrame(0x81, []byte(`{"type":"error","error":{"type":"usage_limit_reached","resets_at":2000000000}}`))
	failed := reconnectTestFrame(0x81, []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","resets_at":2000000000}}}`))
	for _, test := range []struct {
		name          string
		frames        []byte
		want          []byte
		wantReconnect bool
	}{
		{"rejected before admission", limit, nil, true},
		{"error after admission", append(append([]byte(nil), created...), limit...), append(append([]byte(nil), created...), limit...), false},
		{"failed response after admission", append(append([]byte(nil), created...), failed...), append(append([]byte(nil), created...), failed...), false},
		{"failed response without created", failed, failed, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := &reconnectTestExhaustionRecorder{}
			observer := newTerminalResponseObserver("task-1", "test-client", "account-1", router, nil)
			connection := reconnectTestConnection(t, test.frames, "", observer.observeStructuredEvent)
			got, err := io.ReadAll(connection)
			if test.wantReconnect {
				if !errors.Is(err, errCloseWebSocketAfterTerminalLimit) {
					t.Fatalf("expected reconnect, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, test.want) {
				t.Fatalf("forwarded bytes changed: got %q, want %q", got, test.want)
			}
			if router.reportCount != 1 {
				t.Fatalf("exhaustion reports = %d, want 1", router.reportCount)
			}
		})
	}
}

func TestWebSocketCompressedMessagesPreserveFramesAndContext(t *testing.T) {
	var compressed bytes.Buffer
	compressor, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	defer compressor.Close()
	messages := []string{`{"type":"response.created","response":{"id":"response-1"}}`, `{"type":"response.output_text.delta","delta":"response-1 response-1 response-1"}`}
	var frames []byte
	for _, message := range messages {
		compressed.Reset()
		if _, err := compressor.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		if err := compressor.Flush(); err != nil {
			t.Fatal(err)
		}
		payload := compressed.Bytes()
		if !bytes.HasSuffix(payload, []byte{0, 0, 255, 255}) {
			t.Fatal("missing DEFLATE sync-flush suffix")
		}
		frames = append(frames, reconnectTestFrame(0xc1, payload[:len(payload)-4])...)
	}
	var observed []string
	connection := reconnectTestConnection(t, frames, "permessage-deflate", func(message []byte) bool {
		observed = append(observed, string(message))
		return false
	})
	got, err := io.ReadAll(connection)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frames) {
		t.Fatal("compressed frame bytes changed")
	}
	if len(observed) != len(messages) {
		t.Fatalf("observed %d messages, want %d", len(observed), len(messages))
	}
	for index := range messages {
		if observed[index] != messages[index] {
			t.Fatalf("message %d = %q, want %q", index, observed[index], messages[index])
		}
	}
}

// Messages larger than the 32 KiB window, each reusing text from the ones
// before it, decompress exactly while the observer keeps only the window.
func TestWebSocketCompressedMessagesLargerThanTheWindowKeepTheirContext(t *testing.T) {
	var compressed bytes.Buffer
	compressor, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	defer compressor.Close()
	words := func(seed, count int) string {
		var text strings.Builder
		for index := range count {
			fmt.Fprintf(&text, "word%d ", (index*7919+seed)%5003)
		}
		return text.String()
	}
	first := words(1, 9000)
	messages := []string{
		`{"type":"response.create","input":"` + first + `"}`,
		`{"type":"response.create","input":"` + first[len(first)-20000:] + words(2, 300) + `"}`,
		`{"type":"response.create","input":"` + words(3, 40) + first[len(first)-1000:] + `"}`,
		`{"type":"response.create","input":"` + words(4, 20000) + `"}`,
		`{"type":"response.create","input":"short"}`,
	}
	var frames []byte
	for _, message := range messages {
		compressed.Reset()
		if _, err := compressor.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		if err := compressor.Flush(); err != nil {
			t.Fatal(err)
		}
		payload := compressed.Bytes()
		frames = append(frames, reconnectTestFrame(0xc1, payload[:len(payload)-4])...)
	}
	var observed []string
	connection := reconnectTestConnection(t, frames, "permessage-deflate", func(message []byte) bool {
		observed = append(observed, string(message))
		return false
	})
	got, err := io.ReadAll(connection)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frames) {
		t.Fatal("compressed frame bytes changed")
	}
	if len(observed) != len(messages) {
		t.Fatalf("observed %d messages, want %d", len(observed), len(messages))
	}
	for index := range messages {
		if observed[index] != messages[index] {
			t.Fatalf("message %d decompressed wrongly", index)
		}
	}
}

type reconnectTestExhaustionRecorder struct {
	TaskRouter
	reportCount int
}

func (router *reconnectTestExhaustionRecorder) ReportExhaustedTurn(_ store.TaskID, _ store.AccountKey, _ time.Time) string {
	router.reportCount++
	return "moved"
}

func reconnectTestConnection(t *testing.T, frames []byte, extensions string, observer func([]byte) bool) *observedWebSocketConnection {
	t.Helper()
	upstream, downstream := net.Pipe()
	t.Cleanup(func() { upstream.Close(); downstream.Close() })
	upstream.SetDeadline(time.Now().Add(3 * time.Second))
	downstream.SetDeadline(time.Now().Add(3 * time.Second))
	go func() {
		defer upstream.Close()
		upstream.Write(frames)
	}()
	return createObservedWebSocketConnection(downstream, extensions, observer)
}

func reconnectTestFrame(firstByte byte, payload []byte) []byte {
	frame := []byte{firstByte}
	switch {
	case len(payload) < 126:
		frame = append(frame, byte(len(payload)))
	case len(payload) <= 0xffff:
		frame = append(frame, 126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(len(payload)))
	default:
		frame = append(frame, 127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(len(payload)))
	}
	return append(frame, payload...)
}
