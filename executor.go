package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// executorRequestWire mirrors the host's rpcExecutorRequest: ExecutorRequest
// fields serialized under their Go names (pluginapi types carry no json tags)
// plus the streaming coordination extras.
type executorRequestWire struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// streamStartResponseWire mirrors the executor.execute_stream response:
// async mode returns headers only, chunks follow through host.stream.emit.
type streamStartResponseWire struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// streamStartHeaders mirrors the SSE response headers the original server set.
var streamStartHeaders = http.Header{
	"Content-Type":      []string{"text/event-stream; charset=utf-8"},
	"Cache-Control":     []string{"no-cache, no-transform"},
	"X-Accel-Buffering": []string{"no"},
}

// prepareUpstreamCall is the shared preflight of both executor entry points:
// it unwraps the RPC wire payload, resolves the auth view and adapts the
// client body into the upstream request.
func prepareUpstreamCall(requestJSON []byte) (*executorRequestWire, *upstreamRequest, error) {
	wire := &executorRequestWire{}
	if err := json.Unmarshal(requestJSON, wire); err != nil {
		return nil, nil, &ocError{Code: pluginErrorCode, Message: "malformed executor request: " + err.Error()}
	}
	payload := wire.Payload
	if len(payload) == 0 {
		payload = wire.OriginalRequest
	}
	view := parseAuthView(wire.StorageJSON)
	if wire.AuthID != "" {
		view.Holder = wire.AuthID
	}
	built, err := buildUpstreamRequest(context.Background(), payload, stripAuthPrefix(wire.Model, wire.ExecutorRequest), view)
	if err != nil {
		return nil, nil, err
	}
	return wire, built, nil
}

// postUpstreamChat sends the adapted request through the per-auth client.
func postUpstreamChat(built *upstreamRequest) (result *http.Response, err error) {
	request, requestErr := newUpstreamChatRequest(context.Background(), built)
	if requestErr != nil {
		return nil, requestErr
	}
	return clientForAuth(built.ProxyURL).Do(request)
}

// upstreamRetryBackoff waits between transient-upstream-failure retries. It is
// a package var so tests can zero the waits.
var upstreamRetryBackoff = []time.Duration{400 * time.Millisecond, 1200 * time.Millisecond}

// openUpstreamChat posts the adapted request and returns the first response
// that is not a server-side failure. The free-tier upstream intermittently
// answers transport errors or bare HTTP 5xx even for requests it accepts a
// moment later; with only one synthetic auth every surfaced failure also
// cools the auth down in the host, so transient failures are retried here
// (pre-content only, bounded) before they fail the request.
func openUpstreamChat(built *upstreamRequest) (*http.Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		response, err := postUpstreamChat(built)
		switch {
		case err != nil:
			lastErr = upstreamStatusFailure(502, "Request to upstream failed: "+err.Error())
		case response.StatusCode >= http.StatusInternalServerError:
			message := readErrorText(response.Body, streamCtxFrom(built), response.StatusCode, "", &textDecoder{})
			_ = response.Body.Close()
			lastErr = upstreamStatusFailure(response.StatusCode, message)
			response = nil
		default:
			return response, nil
		}
		if attempt >= len(upstreamRetryBackoff) {
			return nil, lastErr
		}
		fsDebugLog(fmt.Sprintf("upstream attempt %d failed (%v), retrying", attempt+1, lastErr))
		time.Sleep(upstreamRetryBackoff[attempt])
	}
}

// upstreamStatusFailure maps a non-2xx upstream response to the oc2cpa error
// semantics: connect/HTTP failures surface with their real status; only the
// actual free-tier rate limit becomes 429.
func upstreamStatusFailure(status int, message string) *ocError {
	return mapUpstreamFailure(status, message).asError()
}

// streamFailErr is the mid-stream failure carrier (502 upstream_stream_error).
func streamFailErr(message string) *ocError {
	return &ocError{Code: "upstream_stream_error", Message: message, HTTPStatus: 502}
}

func is2xx(status int) bool {
	return status >= 200 && status < 300
}

// asPluginError converts response-layer failures into the coded error carrier.
func asPluginError(err error) *ocError {
	if coded, ok := err.(*ocError); ok {
		return coded
	}
	if zen, ok := err.(*zenStreamError); ok {
		return streamFailErr(zen.message)
	}
	return streamFailErr(err.Error())
}

// -----------------------------------------------------------------------------
// executor.execute: aggregate the upstream SSE stream into one JSON object
// -----------------------------------------------------------------------------

func handleExecute(requestJSON []byte) (pluginapi.ExecutorResponse, error) {
	_, built, err := prepareUpstreamCall(requestJSON)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	response, err := openUpstreamChat(built)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if !is2xx(response.StatusCode) {
		message := readErrorText(response.Body, streamCtxFrom(built), response.StatusCode, "", &textDecoder{})
		_ = response.Body.Close()
		return pluginapi.ExecutorResponse{}, upstreamStatusFailure(response.StatusCode, message)
	}
	ctx := streamCtxFrom(built)
	result, err := aggregateUpstreamJSON(response.Body, ctx)
	_ = response.Body.Close()
	if err != nil {
		return pluginapi.ExecutorResponse{}, asPluginError(err)
	}
	raw, err := jsonMarshal(result)
	if err != nil {
		return pluginapi.ExecutorResponse{}, &ocError{Code: pluginErrorCode, Message: err.Error()}
	}
	return pluginapi.ExecutorResponse{
		Payload: raw,
		Headers: http.Header{"X-Request-Id": []string{ctx.requestID}},
	}, nil
}

// -----------------------------------------------------------------------------
// executor.execute_stream
// -----------------------------------------------------------------------------

// streamPump drives the upstream SSE body through parser and writer. It
// signals the RPC handler exactly once — nil when emission started (the caller
// then returns the headers envelope), or the pre-emission failure that must be
// surfaced as a JSON error envelope instead.
//
// This mirrors oc2cpa's send flow: failures before any content go out as a 502
// JSON body; failures after headers are committed go out as an SSE error event
// plus stream close (without [DONE]).
type streamPump struct {
	body     io.ReadCloser
	ctx      *streamCtx
	streamID string
	started  bool
	signaled bool
	ready    chan error
}

func (p *streamPump) signal(err error) {
	if !p.signaled {
		p.signaled = true
		p.ready <- err
	}
}

// emitChunk bridges one encoded SSE frame (or several frames flushed from the
// pending buffer) to the host stream. host.stream.emit must only be called
// once the executor.execute_stream RPC has returned: parallel host calls from
// inside a plugin-call stack deadlock on Windows.
func (p *streamPump) emitChunk(payload []byte) error {
	_, err := goHostCallJson(pluginabi.MethodHostStreamEmit, map[string]any{
		"stream_id": p.streamID,
		"payload":   payload,
	})
	return err
}

// closeStream ends the host stream bridge. The error stays empty because
// failures were already encoded as SSE error events, matching oc2cpa.
func (p *streamPump) closeStream() {
	_, _ = goHostCallJson(pluginabi.MethodHostStreamClose, map[string]any{
		"stream_id": p.streamID,
	})
}

// pumpAbortError marks an emit failure: the downstream is gone, so the pump
// stops reading and closes quietly.
type pumpAbortError struct{ err error }

func (e *pumpAbortError) Error() string { return e.err.Error() }

func (p *streamPump) run() {
	defer func() { _ = p.body.Close() }()
	err := p.pump()
	switch {
	case err == nil:
		return
	case !p.started:
		p.signal(asPluginError(err))
	case isAbort(err):
		fsDebugLog("stream emit failed (downstream gone): " + err.Error())
	default:
		message := err.Error()
		if _, isZen := err.(*zenStreamError); !isZen {
			message = "Upstream stream error"
		}
		payload := errorFrameJSON(p.ctx.sseFrame, message)
		if emitErr := p.emitChunk(payload); emitErr != nil {
			fsDebugLog("stream error frame emit failed: " + emitErr.Error())
		}
		p.closeStream()
	}
}

// errorFrameJSON builds the mid-stream error payload with the same framing
// rule as normal chunks: bare JSON for the native chat-completions writer,
// "data: "-framed for cross-format translators.
func errorFrameJSON(sseFrame bool, message string) []byte {
	raw := streamFailureJSON(message)
	if sseFrame {
		return []byte("data: " + raw)
	}
	return []byte(raw)
}

func isAbort(err error) bool {
	_, ok := err.(*pumpAbortError)
	return ok
}

// pump parses the whole upstream body, encoding and emitting every frame.
func (p *streamPump) pump() error {
	parser := newEventParser(p.body, p.ctx)
	encoder := newStreamEncoder(p.ctx)
	pumpErr := parser.run(func(frame map[string]any) error {
		chunk, encErr := encoder.encode(frame)
		if encErr != nil {
			return encErr
		}
		if len(chunk) == 0 {
			return nil
		}
		if !p.started {
			p.started = true
			p.signal(nil)
		}
		if emitErr := p.emitChunk(chunk); emitErr != nil {
			return &pumpAbortError{err: emitErr}
		}
		return nil
	})
	if pumpErr != nil {
		if isAbort(pumpErr) {
			// The bridge is dead; nothing else to report through it.
			p.closeStream()
		}
		return pumpErr
	}
	done, doneErr := encoder.finish()
	if doneErr != nil {
		// assertCompleted failure: surfaces as a stream error like oc2cpa.
		if !p.started {
			p.started = true
			p.signal(nil)
		}
		payload := errorFrameJSON(p.ctx.sseFrame, asPluginError(doneErr).Message)
		if emitErr := p.emitChunk(payload); emitErr != nil {
			return &pumpAbortError{err: emitErr}
		}
		return &zenStreamError{message: asPluginError(doneErr).Message}
	}
	if !p.started {
		// A complete but content-less stream: return the headers first, then
		// flush the buffered frames and [DONE].
		p.started = true
		p.signal(nil)
	}
	if len(done) > 0 {
		if emitErr := p.emitChunk(done); emitErr != nil {
			return &pumpAbortError{err: emitErr}
		}
	}
	p.closeStream()
	return nil
}

// handleExecuteStream answers executor.execute_stream. It blocks until the
// pump decides the outcome: error envelopes for pre-content failures, headers
// envelope once emission started (the pump keeps streaming afterwards).
func handleExecuteStream(requestJSON []byte) ([]byte, error) {
	wire, built, err := prepareUpstreamCall(requestJSON)
	if err != nil {
		return marshalErrorEnvelopeFrom(err), nil
	}
	response, err := openUpstreamChat(built)
	if err != nil {
		return marshalErrorEnvelopeFrom(err), nil
	}
	if !is2xx(response.StatusCode) {
		message := readErrorText(response.Body, streamCtxFrom(built), response.StatusCode, "", &textDecoder{})
		_ = response.Body.Close()
		return marshalErrorEnvelopeFrom(upstreamStatusFailure(response.StatusCode, message)), nil
	}
	if response.Body == nil {
		return marshalErrorEnvelopeFrom(streamFailErr("Empty response from upstream")), nil
	}
	ctx := streamCtxFrom(built)
	ctx.sseFrame = clientNeedsSSEFrame(wire.Metadata)
	pump := &streamPump{
		body:     response.Body,
		ctx:      ctx,
		streamID: wire.StreamID,
		ready:    make(chan error, 1),
	}
	go pump.run()
	if pumpErr := <-pump.ready; pumpErr != nil {
		return marshalErrorEnvelopeFrom(pumpErr), nil
	}
	return marshalOKEnvelope(streamStartResponseWire{Headers: streamStartHeaders}), nil
}

// marshalErrorEnvelopeFrom converts the coded error carrier into RPC bytes.
// An error envelope with rc=0 tells the host loader this is a plugin-generated
// client-visible error rather than an internal plugin fault.
func marshalErrorEnvelopeFrom(err error) []byte {
	code := pluginErrorCode
	status := 0
	if coded, ok := err.(*ocError); ok {
		code = coded.Code
		status = coded.HTTPStatus
	}
	return marshalErrorEnvelope(code, err.Error(), status)
}
