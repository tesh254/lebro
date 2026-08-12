package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// maxStreamEventBytes bounds one Server-Sent Event frame. The server emits
// compact JSON on a single data line, so a frame far past this is a malfunction
// rather than a large run, and reading it without a bound would let a server
// exhaust the client's memory.
const maxStreamEventBytes = 1 << 20

// ClientStream is the handle returned by Client.RunStream. It mirrors
// lebro.StreamRun deliberately: the caller drains Events until it is closed,
// then calls Wait to collect the terminal result and error. Cancel stops the
// remote run and releases the reader goroutine even when the caller abandons
// the stream before draining it.
//
// The shape is the same as the in-process one so moving an agent behind HTTP
// does not change how a caller consumes it:
//
//	stream, err := client.RunStream(ctx, "assistant", req)
//	if err != nil {
//	    return err
//	}
//	defer stream.Cancel()
//	for event := range stream.Events {
//	    fmt.Print(event.Text)
//	}
//	result, err := stream.Wait()
//
// Events carries only model deltas. The terminal event is consumed by the
// stream itself and surfaces through Wait, so a caller cannot mistake it for
// another delta or miss it by breaking out of the loop early.
type ClientStream struct {
	// Events receives each model delta in order and is closed when the stream
	// ends, for any reason. A caller must drain it before calling Wait, or call
	// Drain, which does both.
	Events <-chan StreamEvent

	// ctx is the stream's own context, cancelled by Cancel. The reader selects
	// on it when forwarding a delta so an abandoned consumer cannot park the
	// goroutine on a channel nobody reads.
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	// done is closed by the reader goroutine after outcome is final, so Wait
	// observes a fully written outcome without a lock.
	done     chan struct{}
	outcome  RunResponse
	outcome_ error
}

// Cancel stops the remote run and releases the reader goroutine. It is safe to
// call multiple times and after Wait has returned, so `defer stream.Cancel()`
// is always correct.
//
// Cancelling closes the connection, which the server observes as a client
// disconnect and translates into a cancelled run: the run stops rather than
// continuing unobserved to completion.
func (s *ClientStream) Cancel() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// Wait blocks until the stream has ended and returns the run's terminal result.
// It must be called after Events has been fully drained, because the reader
// goroutine cannot finish while it is blocked sending a delta nobody is
// receiving.
//
// A run the server reported as failed or cancelled returns an *APIError
// carrying the terminal event's code, so a streamed failure is classified
// exactly as the non-streaming route would have classified it. A stream that
// ended without a terminal event returns an error wrapping ErrStreamIncomplete:
// the run's outcome is unknown, which is not the same as a run that failed.
func (s *ClientStream) Wait() (RunResponse, error) {
	if s == nil {
		return RunResponse{}, errors.New("lebro/httpapi: stream is nil")
	}
	<-s.done
	return s.outcome, s.outcome_
}

// Drain reads Events to completion and returns the terminal outcome. It is the
// canonical way to collect the final result when the caller does not need to
// inspect each delta.
func (s *ClientStream) Drain() (RunResponse, error) {
	if s == nil {
		return RunResponse{}, errors.New("lebro/httpapi: stream is nil")
	}
	for range s.Events {
	}
	return s.Wait()
}

// RunStream executes an agent remotely and streams its output as ordered
// events. Errors discovered before the stream opens — an unknown agent, a
// malformed request, a thread that cannot be resolved — are returned here as an
// *APIError; once the stream is open, a failure is reported through Wait.
//
// The caller must call Cancel when finished, normally with defer, so the
// connection and the reader goroutine are released even when the stream is
// abandoned before it completes. Cancelling ctx has the same effect.
func (c *Client) RunStream(ctx context.Context, agentID string, request RunRequest, options ...RunOption) (*ClientStream, error) {
	if c == nil {
		return nil, errors.New("lebro/httpapi: client is nil")
	}
	if agentID == "" {
		return nil, errors.New("lebro/httpapi: agent ID is required")
	}

	// The stream's lifetime is the handle's, not the call's: the response body
	// is read after RunStream returns, so the request context must outlive it.
	// Cancel is what ends it, and every failure path below calls cancel before
	// returning so an early error cannot leak the context.
	streamCtx, cancel := context.WithCancel(ctx)

	path, escaped := agentRunPath(agentID, "/stream")
	response, err := c.do(streamCtx, http.MethodPost, path, escaped, queryFrom(options), request)
	if err != nil {
		cancel()
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		defer closeBody(response)
		cancel()
		body, readErr := readBody(response)
		if readErr != nil {
			return nil, readErr
		}
		if apiErr := errorForResponse(response, body); apiErr != nil {
			return nil, apiErr
		}
		// A non-200 the error mapper accepted as success cannot happen through
		// errorForResponse, but a status outside 2xx with no decodable body
		// must still fail rather than return a stream that never yields.
		return nil, apiErrorFromStatus(response.StatusCode)
	}

	events := make(chan StreamEvent)
	stream := &ClientStream{
		Events: events,
		ctx:    streamCtx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go stream.read(response, events)
	return stream, nil
}

// read consumes the response body, forwarding deltas and recording the terminal
// outcome. It owns the response body and the events channel: both are closed
// before done is closed, so a caller that has observed Wait knows the
// connection is released.
func (s *ClientStream) read(response *http.Response, events chan<- StreamEvent) {
	defer close(s.done)
	defer func() {
		close(events)
		closeBody(response)
	}()

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 4096), maxStreamEventBytes)

	var (
		name      string
		data      []byte
		terminal  bool
		sawResult bool
	)

	// flush handles one complete frame. It reports false when the caller should
	// stop reading: a terminal event was seen, or the consumer went away.
	flush := func() bool {
		defer func() {
			name = ""
			data = nil
		}()
		if len(data) == 0 {
			return true
		}

		var event StreamEvent
		if err := json.Unmarshal(data, &event); err != nil {
			s.outcome_ = fmt.Errorf("lebro/httpapi: decode stream event: %w", ErrMalformedResponse)
			return false
		}
		// The event name is authoritative: Type mirrors it, but a server that
		// omitted Type would otherwise make every frame look like a delta.
		if event.Type == "" {
			event.Type = name
		}

		if isTerminalEvent(name) {
			terminal = true
			sawResult = true
			s.outcome, s.outcome_ = terminalOutcome(event)
			return false
		}

		// A caller that abandons the stream without draining Events — breaking
		// out of the range loop, or never starting one — leaves nobody to
		// receive this send. Selecting on the stream context means Cancel
		// releases this goroutine instead of parking it forever on a channel
		// with no reader, which is what the deferred Cancel in the documented
		// usage relies on.
		select {
		case events <- event:
			return true
		case <-s.ctx.Done():
			s.outcome_ = streamReadError(s.ctx.Err())
			return false
		}
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		switch {
		case len(bytes.TrimSpace(line)) == 0:
			// A blank line ends a frame.
			if !flush() {
				return
			}
		case bytes.HasPrefix(line, []byte("event:")):
			name = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			// Multiple data lines in one frame concatenate with a newline, per
			// the SSE specification. The server emits a single line, but a
			// proxy is permitted to re-wrap and a client that assumed one line
			// would silently truncate the payload.
			//
			// append copies into data's own array rather than retaining the
			// slice, which matters because scanner.Bytes points into a buffer
			// the scanner overwrites on the next Scan. A multi-line frame is
			// held across iterations, so retaining the slice would decode
			// whatever the scanner read next.
			chunk := bytes.TrimPrefix(bytes.TrimPrefix(line[len("data:"):], []byte(" ")), []byte("\t"))
			// The scanner's own limit bounds one physical line, not the frame
			// they accumulate into, so a server sending many under-limit data
			// lines in a single frame would otherwise make the client allocate
			// without bound. Enforce the limit on the accumulated payload.
			if len(data)+len(chunk)+1 > maxStreamEventBytes {
				s.outcome_ = fmt.Errorf("lebro/httpapi: stream event exceeds %d bytes: %w", maxStreamEventBytes, ErrMalformedResponse)
				return
			}
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		case bytes.HasPrefix(line, []byte(":")):
			// A comment frame, used for keep-alives. Ignored.
		default:
			// An unknown field. The specification says to ignore it rather than
			// fail, so a server that adds one stays readable by this client.
		}
	}

	// A frame the server wrote without a trailing blank line — which happens
	// when the connection closes right after the terminal event — is still a
	// complete frame and must be processed.
	if len(data) > 0 && !terminal {
		if !flush() {
			return
		}
	}

	if err := scanner.Err(); err != nil {
		if s.outcome_ == nil {
			s.outcome_ = streamReadError(err)
		}
		return
	}
	if !sawResult && s.outcome_ == nil {
		s.outcome_ = ErrStreamIncomplete
	}
}

// streamReadError classifies a failure to read the stream body. A cancelled
// context is reported as a cancelled run rather than a transport fault: the
// caller stopped the run, and the server saw the same thing.
func streamReadError(err error) error {
	// The specific context error is carried through rather than collapsed, so a
	// deadline stays distinguishable from an explicit cancel.
	if errors.Is(err, context.Canceled) {
		return cancelledAPIError(context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return cancelledAPIError(context.DeadlineExceeded)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("lebro/httpapi: %w", ErrStreamIncomplete)
	}
	return fmt.Errorf("lebro/httpapi: read stream: %w", err)
}

// isTerminalEvent reports whether an event name ends the stream. The stream
// contract guarantees exactly one of these, so recognizing them is what lets
// Wait distinguish a completed run from a dropped connection.
func isTerminalEvent(name string) bool {
	switch name {
	case eventSucceeded, eventFailed, eventCancelled:
		return true
	default:
		return false
	}
}

// terminalOutcome projects a terminal event onto the result and error a caller
// receives from Wait. A terminal event carrying an error yields an *APIError
// with no status: the HTTP response was 200 and committed before the run
// failed, so there is no status to report and claiming one would be a fiction.
func terminalOutcome(event StreamEvent) (RunResponse, error) {
	result := RunResponse{
		RunID:            event.RunID,
		Status:           event.Status,
		Content:          event.Text,
		StructuredOutput: event.StructuredOutput,
	}
	if event.Error != nil {
		return result, apiErrorFromBody(*event.Error, 0)
	}
	return result, nil
}
