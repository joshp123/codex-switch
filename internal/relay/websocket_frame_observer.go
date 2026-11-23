package relay

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
)

var errCloseWebSocketAfterTerminalLimit = errors.New("close WebSocket after terminal provider limit event")

const (
	websocketContinuationOpcode = 0x0
	websocketTextOpcode         = 0x1
	websocketBinaryOpcode       = 0x2
	websocketMaximumDictionary  = 32 << 10
	// A connection keeps buffers up to this size between messages. A larger
	// one, such as a long task's whole context in one request, is released
	// once its message is handled, so an idle connection holds little.
	websocketRetainedBufferBytes = 64 << 10
)

// deflateMessageTail ends a permessage-deflate message for the decompressor.
var deflateMessageTail = []byte{0x00, 0x00, 0xff, 0xff}

type observedWebSocketConnection struct {
	io.ReadWriteCloser
	frameObserver        websocketServerFrameObserver
	pendingFrames        bytes.Buffer
	readyFrames          *bytes.Reader
	closeOnce            sync.Once
	closeError           error
	negotiatedExtensions string
	clientFrameObserver  *websocketServerFrameObserver
	clientInput          []byte
	clientPendingFrames  bytes.Buffer
	reportReadError      func(error)
}

func createObservedWebSocketConnection(
	connection io.ReadWriteCloser,
	negotiatedExtensions string,
	onEvent func([]byte) bool,
) *observedWebSocketConnection {
	return &observedWebSocketConnection{
		ReadWriteCloser:      connection,
		negotiatedExtensions: negotiatedExtensions,
		frameObserver: websocketServerFrameObserver{
			onMessage:               onEvent,
			perMessageDeflate:       headerContainsExtension(negotiatedExtensions, "permessage-deflate"),
			serverNoContextTakeover: strings.Contains(strings.ToLower(negotiatedExtensions), "server_no_context_takeover"),
		},
	}
}

func (connection *observedWebSocketConnection) Read(destination []byte) (int, error) {
	if connection.readyFrames != nil && connection.readyFrames.Len() > 0 {
		n, err := connection.readyFrames.Read(destination)
		if connection.readyFrames.Len() == 0 {
			connection.readyFrames = nil
		}
		return n, err
	}
	connection.readyFrames = nil
	for {
		frame, err := readUpstreamWebSocketFrame(connection.ReadWriteCloser)
		if err != nil {
			if connection.reportReadError != nil {
				connection.reportReadError(err)
			}
			return 0, err
		}
		if frame[0]&0x08 != 0 {
			connection.readyFrames = bytes.NewReader(frame)
			n, err := connection.readyFrames.Read(destination)
			if connection.readyFrames.Len() == 0 {
				connection.readyFrames = nil
			}
			return n, err
		}
		connection.pendingFrames.Write(frame)
		if err := connection.frameObserver.ObserveFrames(frame); err != nil && connection.reportReadError != nil {
			connection.reportReadError(err)
		}
		if connection.frameObserver.TerminalLimitObserved() {
			// The provider rejected this response. Withhold its terminal limit and
			// reconnect so Codex supplies continuation on the newly routed account.
			// Successful response messages have already passed through unchanged.
			return 0, errCloseWebSocketAfterTerminalLimit
		}
		if !connection.frameObserver.messageActive {
			connection.readyFrames = bytes.NewReader(connection.pendingFrames.Bytes())
			connection.pendingFrames = bytes.Buffer{}
			n, err := connection.readyFrames.Read(destination)
			if connection.readyFrames.Len() == 0 {
				connection.readyFrames = nil
			}
			return n, err
		}
	}
}

func readUpstreamWebSocketFrame(source io.Reader) ([]byte, error) {
	var frame bytes.Buffer
	var header [2]byte
	if _, err := io.ReadFull(source, header[:]); err != nil {
		return nil, err
	}
	frame.Write(header[:])
	payloadLength := int64(header[1] & 0x7f)
	switch payloadLength {
	case 126:
		var length [2]byte
		if _, err := io.ReadFull(source, length[:]); err != nil {
			return nil, err
		}
		frame.Write(length[:])
		payloadLength = int64(binary.BigEndian.Uint16(length[:]))
	case 127:
		var length [8]byte
		if _, err := io.ReadFull(source, length[:]); err != nil {
			return nil, err
		}
		frame.Write(length[:])
		payloadLength = int64(binary.BigEndian.Uint64(length[:]))
		if payloadLength < 0 {
			return nil, errors.New("invalid WebSocket payload length")
		}
	}
	if header[1]&0x80 != 0 {
		if payloadLength > (1<<63-1)-4 {
			return nil, errors.New("invalid WebSocket payload length")
		}
		payloadLength += 4
	}
	if _, err := io.CopyN(&frame, source, payloadLength); err != nil {
		return nil, err
	}
	return frame.Bytes(), nil
}

func (connection *observedWebSocketConnection) Close() error {
	connection.closeOnce.Do(func() {
		connection.closeError = connection.ReadWriteCloser.Close()
	})
	return connection.closeError
}

func (connection *observedWebSocketConnection) CloseWrite() error {
	if halfCloser, ok := connection.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return halfCloser.CloseWrite()
	}
	return nil
}

func headerContainsExtension(headerValue, extensionName string) bool {
	for extension := range strings.SplitSeq(headerValue, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(extension), ";")
		if strings.EqualFold(strings.TrimSpace(name), extensionName) {
			return true
		}
	}
	return false
}

type websocketServerFrameObserver struct {
	headerBytes      [14]byte
	headerLength     int
	headerNeeded     int
	payloadRemaining uint64
	payloadMask      [4]byte
	payloadMaskIndex uint64
	frameOpcode      byte
	frameFinal       bool

	messageActive     bool
	messageCompressed bool
	messageBytes      []byte
	onMessage         func([]byte) bool
	terminalLimit     bool

	perMessageDeflate       bool
	serverNoContextTakeover bool
	compressionDictionary   []byte
}

func (observer *websocketServerFrameObserver) ObserveFrames(frameBytes []byte) error {
	for len(frameBytes) > 0 {
		if observer.headerNeeded == 0 && observer.payloadRemaining == 0 {
			observer.headerNeeded = 2
		}
		if observer.headerLength < observer.headerNeeded {
			copied := min(len(frameBytes), observer.headerNeeded-observer.headerLength)
			copy(observer.headerBytes[observer.headerLength:], frameBytes[:copied])
			observer.headerLength += copied
			frameBytes = frameBytes[copied:]
			if observer.headerLength < observer.headerNeeded {
				continue
			}
			if observer.headerNeeded == 2 {
				observer.headerNeeded += websocketExtendedHeaderLength(observer.headerBytes[1])
				if observer.headerLength < observer.headerNeeded {
					continue
				}
			}
			observer.beginFrame()
			if observer.payloadRemaining == 0 {
				if err := observer.finishFrame(); err != nil {
					return err
				}
				continue
			}
		}

		payloadLength := len(frameBytes)
		if uint64(payloadLength) > observer.payloadRemaining {
			payloadLength = int(observer.payloadRemaining)
		}
		observer.observeFramePayload(frameBytes[:payloadLength])
		frameBytes = frameBytes[payloadLength:]
		observer.payloadRemaining -= uint64(payloadLength)
		if observer.payloadRemaining == 0 {
			if err := observer.finishFrame(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (observer *websocketServerFrameObserver) TerminalLimitObserved() bool {
	return observer.terminalLimit
}

func websocketExtendedHeaderLength(secondHeaderByte byte) int {
	extraLength := 0
	switch secondHeaderByte & 0x7f {
	case 126:
		extraLength = 2
	case 127:
		extraLength = 8
	}
	if secondHeaderByte&0x80 != 0 {
		extraLength += 4
	}
	return extraLength
}

func (observer *websocketServerFrameObserver) beginFrame() {
	observer.frameFinal = observer.headerBytes[0]&0x80 != 0
	observer.frameOpcode = observer.headerBytes[0] & 0x0f
	payloadLengthCode := observer.headerBytes[1] & 0x7f
	headerIndex := 2
	switch payloadLengthCode {
	case 126:
		observer.payloadRemaining = uint64(binary.BigEndian.Uint16(observer.headerBytes[headerIndex : headerIndex+2]))
		headerIndex += 2
	case 127:
		observer.payloadRemaining = binary.BigEndian.Uint64(observer.headerBytes[headerIndex : headerIndex+8])
		headerIndex += 8
	default:
		observer.payloadRemaining = uint64(payloadLengthCode)
	}
	if observer.headerBytes[1]&0x80 != 0 {
		copy(observer.payloadMask[:], observer.headerBytes[headerIndex:headerIndex+4])
	} else {
		observer.payloadMask = [4]byte{}
	}
	observer.payloadMaskIndex = 0
	if observer.frameOpcode == websocketTextOpcode || observer.frameOpcode == websocketBinaryOpcode {
		observer.messageActive = true
		observer.messageCompressed = observer.perMessageDeflate && observer.headerBytes[0]&0x40 != 0
		observer.messageBytes = observer.messageBytes[:0]
	}
}

func (observer *websocketServerFrameObserver) observeFramePayload(payload []byte) {
	if !observer.messageActive || observer.frameOpcode != websocketTextOpcode &&
		observer.frameOpcode != websocketBinaryOpcode && observer.frameOpcode != websocketContinuationOpcode {
		return
	}
	start := len(observer.messageBytes)
	observer.messageBytes = append(observer.messageBytes, payload...)
	if observer.headerBytes[1]&0x80 != 0 {
		decoded := observer.messageBytes[start:]
		for index := range decoded {
			decoded[index] ^= observer.payloadMask[observer.payloadMaskIndex%4]
			observer.payloadMaskIndex++
		}
	}
}

func (observer *websocketServerFrameObserver) finishFrame() error {
	defer func() {
		observer.headerLength = 0
		observer.headerNeeded = 0
		observer.payloadRemaining = 0
	}()
	if !observer.messageActive || !observer.frameFinal || observer.frameOpcode != websocketTextOpcode &&
		observer.frameOpcode != websocketBinaryOpcode && observer.frameOpcode != websocketContinuationOpcode {
		return nil
	}

	defer func() {
		observer.messageActive = false
		observer.messageCompressed = false
		observer.messageBytes = reuseOrRelease(observer.messageBytes)
	}()

	message := observer.messageBytes
	if observer.messageCompressed {
		decompressed, err := observer.decompressMessage(message)
		if err != nil {
			return err
		}
		message = decompressed
	}
	if observer.onMessage != nil && observer.onMessage(message) {
		observer.terminalLimit = true
	}
	return nil
}

func (observer *websocketServerFrameObserver) decompressMessage(compressed []byte) ([]byte, error) {
	compressedWithTail := io.MultiReader(bytes.NewReader(compressed), bytes.NewReader(deflateMessageTail))
	var decompressor io.ReadCloser
	if len(observer.compressionDictionary) > 0 {
		decompressor = flate.NewReaderDict(compressedWithTail, observer.compressionDictionary)
	} else {
		decompressor = flate.NewReader(compressedWithTail)
	}
	decompressed, err := io.ReadAll(decompressor)
	closeError := decompressor.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if closeError != nil && !errors.Is(closeError, io.ErrUnexpectedEOF) {
		return nil, closeError
	}
	if observer.serverNoContextTakeover {
		observer.compressionDictionary = observer.compressionDictionary[:0]
	} else {
		observer.compressionDictionary = slideDeflateWindow(observer.compressionDictionary, decompressed)
	}
	return decompressed, nil
}

// slideDeflateWindow keeps the last 32 KiB of decompressed output, the most
// a later message can refer back to, in a buffer that never grows past it.
func slideDeflateWindow(window, output []byte) []byte {
	if window == nil {
		window = make([]byte, 0, websocketMaximumDictionary)
	}
	if len(output) >= websocketMaximumDictionary {
		return append(window[:0], output[len(output)-websocketMaximumDictionary:]...)
	}
	if excess := len(window) + len(output) - websocketMaximumDictionary; excess > 0 {
		copy(window, window[excess:])
		window = window[:len(window)-excess]
	}
	return append(window, output...)
}

// reuseOrRelease empties a buffer for the next message, keeping its memory
// only when it is small.
func reuseOrRelease(buffer []byte) []byte {
	if cap(buffer) > websocketRetainedBufferBytes {
		return nil
	}
	return buffer[:0]
}

func (connection *observedWebSocketConnection) observeClientMessages(onMessage func([]byte) bool) {
	connection.clientFrameObserver = &websocketServerFrameObserver{
		onMessage:               onMessage,
		perMessageDeflate:       headerContainsExtension(connection.negotiatedExtensions, "permessage-deflate"),
		serverNoContextTakeover: strings.Contains(strings.ToLower(connection.negotiatedExtensions), "client_no_context_takeover"),
	}
}

func (connection *observedWebSocketConnection) Write(data []byte) (int, error) {
	if connection.clientFrameObserver == nil {
		return connection.ReadWriteCloser.Write(data)
	}
	connection.clientInput = append(connection.clientInput, data...)
	for len(connection.clientInput) > 0 {
		frameLength, err := completeWebSocketFrameLength(connection.clientInput)
		if err != nil {
			return 0, err
		}
		if frameLength == 0 {
			break
		}
		frame := connection.clientInput[:frameLength]
		connection.clientInput = connection.clientInput[frameLength:]
		// Control frames must pass even while a fragmented message is buffered.
		if frame[0]&0x08 != 0 {
			if err := writeAllWebSocketBytes(connection.ReadWriteCloser, frame); err != nil {
				return 0, err
			}
			continue
		}
		bufferedMessage := connection.clientPendingFrames.Len() > 0
		if err := connection.clientFrameObserver.ObserveFrames(frame); err != nil {
			return 0, err
		}
		if connection.clientFrameObserver.TerminalLimitObserved() {
			return 0, errCloseWebSocketAfterTerminalLimit
		}
		if connection.clientFrameObserver.messageActive || bufferedMessage {
			connection.clientPendingFrames.Write(frame)
		}
		if !connection.clientFrameObserver.messageActive {
			messageFrames := frame
			if bufferedMessage {
				messageFrames = connection.clientPendingFrames.Bytes()
			}
			if err := writeAllWebSocketBytes(connection.ReadWriteCloser, messageFrames); err != nil {
				return 0, err
			}
			if connection.clientPendingFrames.Cap() > websocketRetainedBufferBytes {
				connection.clientPendingFrames = bytes.Buffer{}
			} else {
				connection.clientPendingFrames.Reset()
			}
		}
	}
	if len(connection.clientInput) == 0 {
		connection.clientInput = reuseOrRelease(connection.clientInput)
	} else if cap(connection.clientInput) > websocketRetainedBufferBytes {
		// A partial frame stays; the large buffer behind consumed frames goes.
		connection.clientInput = append([]byte(nil), connection.clientInput...)
	}
	return len(data), nil
}

func writeAllWebSocketBytes(destination io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := destination.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func completeWebSocketFrameLength(data []byte) (int, error) {
	if len(data) < 2 {
		return 0, nil
	}
	headerLength := 2 + websocketExtendedHeaderLength(data[1])
	if len(data) < headerLength {
		return 0, nil
	}
	payloadLength := uint64(data[1] & 0x7f)
	switch payloadLength {
	case 126:
		payloadLength = uint64(binary.BigEndian.Uint16(data[2:4]))
	case 127:
		payloadLength = binary.BigEndian.Uint64(data[2:10])
	}
	if payloadLength > uint64(int(^uint(0)>>1)-headerLength) {
		return 0, errors.New("invalid WebSocket payload length")
	}
	frameLength := headerLength + int(payloadLength)
	if len(data) < frameLength {
		return 0, nil
	}
	return frameLength, nil
}
