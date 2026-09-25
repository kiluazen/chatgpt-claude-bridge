package proxy

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kiluazen/chatgpt-claude-bridge/router/internal/upstream"
)

// Codex keeps a websocket open to /responses and sends it one response.create
// message at a time. Each is answered by one response's events, the same JSON
// an HTTP request streams.
//
// The router dials OpenAI's websocket before it answers Codex's handshake, so
// Codex gets OpenAI's own answer: its refusal when it refuses, and when it
// accepts, its headers, such as sticky routing. A message for an OpenAI model
// then goes down OpenAI's socket, and OpenAI's frames come back as they are.
// A message for another model is answered by that model's upstream over HTTP,
// and its events come back as frames, the way OpenAI's socket sends them.

// handshakeHeaders belong to one websocket handshake, not to what it asks
// for; each side of the router makes its own.
var handshakeHeaders = []string{"Host", "Connection", "Upgrade", "Content-Length", "Sec-Websocket-Key",
	"Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Accept", "Sec-Websocket-Protocol"}

func withoutHandshake(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range handshakeHeaders {
		out.Del(k)
	}
	return out
}

func (s *Server) serveResponsesWebsocket(w http.ResponseWriter, r *http.Request) {
	native, err := s.nativeURL(r.URL)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	header := withoutHandshake(r.Header)
	if _, ok := header["User-Agent"]; !ok {
		header["User-Agent"] = []string{""} // none, as Codex sent, rather than Go's
	}
	up, resp, err := s.dialNative(r.Context(), native.String(), header)
	if err != nil {
		status := refuse(w, resp, err)
		slog.Info("websocket", "path", r.URL.Path, "status", status, "err", err)
		return
	}
	for k, v := range withoutHandshake(resp.Header) {
		w.Header()[k] = v
	}
	// Codex and the router share a machine, so their frames go uncompressed.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		up.CloseNow()
		return
	}
	client.SetReadLimit(maxBodyBytes)

	sess := &wsSession{s: s, client: client, url: native.String(), header: header,
		upDone: make(chan upEnd, 1), done: make(chan struct{})}
	s.sessions.Store(sess, struct{}{})
	defer s.sessions.Delete(sess)
	if s.draining.Load() {
		sess.drain()
	}
	start := time.Now()
	sess.attach(up)
	sess.run(r.Context())
	slog.Info("websocket", "path", r.URL.Path, "duration", time.Since(start).Round(time.Millisecond))
}

// dialNative opens OpenAI's websocket with Codex's headers. When OpenAI
// refuses, the response it returns has OpenAI's whole body.
func (s *Server) dialNative(ctx context.Context, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t := &keepRefusal{RoundTripper: s.transport}
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: t},
		HTTPHeader: header,
		// Codex asks OpenAI for compressed frames, so the router does too.
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		if resp != nil && t.body != nil {
			resp.Body = io.NopCloser(bytes.NewReader(t.body))
		}
		return nil, resp, err
	}
	conn.SetReadLimit(maxBodyBytes)
	return conn, resp, nil
}

// keepRefusal keeps the whole body of a refused handshake, of which Dial
// returns only the first 1024 bytes. Codex reads OpenAI's errors, such as a
// usage limit's, from that body.
type keepRefusal struct {
	http.RoundTripper
	body []byte
}

func (k *keepRefusal) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := k.RoundTripper.RoundTrip(req)
	if err != nil || resp.StatusCode == http.StatusSwitchingProtocols {
		return resp, err
	}
	k.body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(k.body))
	return resp, nil
}

// refuse answers Codex's handshake the way OpenAI answered the router's, and
// returns the status it sent.
func refuse(w http.ResponseWriter, resp *http.Response, err error) int {
	if resp == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		// OpenAI was unreachable, or accepted in a way the router could not use.
		writeError(w, http.StatusBadGateway, err.Error())
		return http.StatusBadGateway
	}
	for k, v := range withoutHandshake(resp.Header) {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	return resp.StatusCode
}

// wsSession is one websocket from Codex.
type wsSession struct {
	s      *Server
	client *websocket.Conn
	url    string      // OpenAI's websocket
	header http.Header // Codex's handshake, to dial OpenAI again

	up     *websocket.Conn // OpenAI's socket, while this one's messages go there
	upDone chan upEnd      // a relay reports that OpenAI's socket ended
	done   chan struct{}   // closed when the session ends

	cont continuation // the last response the router answered itself

	mu       sync.Mutex
	current  *wsResponse // the response in flight
	draining bool        // the router is stopping; close once nothing is in flight
}

type upEnd struct {
	conn *websocket.Conn
	err  error
}

// continuation is what a message continuing the router's last response
// builds on: that response's whole input, then its output.
type continuation struct {
	responseID string
	items      []json.RawMessage
}

// wsResponse is a response in flight, for the log.
type wsResponse struct {
	model string
	route upstream.Route
	start time.Time
}

// errDraining ends a session that got a message after the router began to
// stop. Codex sends the message again on a new websocket.
var errDraining = errors.New("router is stopping")

func (sess *wsSession) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		if sess.up != nil {
			sess.up.CloseNow()
		}
		sess.client.CloseNow()
		sess.end("websocket closed")
		close(sess.done)
	}()
	frames := make(chan []byte)
	go func() {
		defer cancel()
		for {
			typ, data, err := sess.client.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			select {
			case frames <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case end := <-sess.upDone:
			if end.conn == sess.up {
				// OpenAI closed the socket this one's messages go to; Codex
				// learns it the same way and opens another.
				sess.client.Close(closeOf(end.err))
				return
			}
		case frame := <-frames:
			if err := sess.handle(ctx, frame); err != nil {
				if errors.Is(err, errDraining) {
					sess.client.Close(websocket.StatusServiceRestart, "router restarting")
				} else if ctx.Err() == nil {
					slog.Error("websocket", "err", err)
				}
				return
			}
		}
	}
}

// handle serves one message from Codex. An error ends the session.
func (sess *wsSession) handle(ctx context.Context, frame []byte) error {
	route := upstream.Native
	req, err := upstream.Parse(frame)
	if err == nil && req.Type() == "response.create" {
		route = sess.s.route(req.Model())
	} else {
		req = nil // not a message the router reads; OpenAI gets it as it is
	}
	if route == upstream.Native {
		return sess.relay(ctx, frame, req)
	}
	return sess.serveOther(ctx, route, req)
}

// relay sends a message down OpenAI's socket, dialing it again if this
// session's last messages were for other models.
func (sess *wsSession) relay(ctx context.Context, frame []byte, req *upstream.Request) error {
	if req != nil && !sess.begin(req.Model(), upstream.Native) {
		return errDraining
	}
	if sess.up == nil {
		header := sess.header.Clone()
		// Sticky routing from the handshake belongs to an earlier turn; each
		// message carries the current one.
		header.Del("X-Codex-Turn-State")
		up, resp, err := sess.s.dialNative(ctx, sess.url, header)
		if err != nil {
			if resp == nil || resp.StatusCode == http.StatusSwitchingProtocols {
				return sess.failMessage(ctx, http.StatusBadGateway, "", err.Error())
			}
			body, _ := io.ReadAll(resp.Body)
			return sess.failResponse(ctx, resp.StatusCode, resp.Header, body)
		}
		sess.attach(up)
	}
	if req != nil {
		fixed, changed, err := req.ForNative()
		if err != nil {
			slog.Warn("websocket message sent as it came", "model", req.Model(), "err", err)
		} else if changed {
			frame = fixed
		}
	}
	sess.cont = continuation{} // OpenAI holds the next continuation
	return sess.up.Write(ctx, websocket.MessageText, frame)
}

// attach relays OpenAI's frames to Codex until OpenAI's socket ends.
func (sess *wsSession) attach(up *websocket.Conn) {
	sess.up = up
	go func() {
		ctx := context.Background()
		var err error
		for {
			var typ websocket.MessageType
			var data []byte
			if typ, data, err = up.Read(ctx); err != nil {
				break
			}
			// The response a frame ends is the one in flight when it arrives:
			// once Codex has the frame, its next message may begin another.
			var cur *wsResponse
			t := eventType(data)
			if isTerminal(t) {
				cur = sess.inFlightResponse()
			}
			if err = sess.client.Write(ctx, typ, data); err != nil {
				up.CloseNow()
				break
			}
			if cur != nil {
				sess.endResponse(cur, t)
			}
		}
		select {
		case sess.upDone <- upEnd{up, err}:
		case <-sess.done:
		}
	}()
}

// detach stops sending this session's messages to OpenAI's socket. It is
// dialed again if an OpenAI model comes back.
func (sess *wsSession) detach() {
	if up := sess.up; up != nil {
		sess.up = nil
		go up.Close(websocket.StatusNormalClosure, "")
	}
}

// serveOther answers a message for a model served outside OpenAI. Its
// upstream gets, over HTTP, the whole request the message stands for.
func (sess *wsSession) serveOther(ctx context.Context, route upstream.Route, req *upstream.Request) error {
	model := req.Model()
	if !sess.begin(model, route) {
		return errDraining
	}
	sess.detach()
	ws := req.Websocket()
	var before []json.RawMessage
	if ws.PreviousResponseID != "" {
		if ws.PreviousResponseID != sess.cont.responseID {
			// Codex answers this by sending the whole request.
			return sess.failMessage(ctx, http.StatusBadRequest, "previous_response_not_found", "Previous response not found.")
		}
		before = sess.cont.items
	}
	sess.cont = continuation{}
	if err := req.Standalone(before); err != nil {
		return sess.failMessage(ctx, http.StatusBadRequest, "", err.Error())
	}
	input, err := req.Input()
	if err != nil {
		return sess.failMessage(ctx, http.StatusBadRequest, "", err.Error())
	}
	if !ws.Generate {
		return sess.prewarm(ctx, model, input)
	}

	f := forwarding{route: route, model: model, status: http.StatusBadRequest}
	if err := sess.s.forExternal(&f, req, req.Body()); err != nil {
		return sess.failMessage(ctx, f.status, "", err.Error())
	}
	if f.compaction {
		return sess.compact(ctx, f, input)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url.String(), bytes.NewReader(f.body))
	if err != nil {
		return err
	}
	httpReq.Header = f.externalHeader("", "")
	resp, err := sess.s.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return sess.failMessage(ctx, http.StatusBadGateway, "", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return sess.failResponse(ctx, resp.StatusCode, resp.Header, body)
	}

	var id, terminal string
	var output []json.RawMessage
	var writeErr error
	readErr := readEvents(resp.Body, func(data []byte) bool {
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal(data, &ev) != nil || ev.Type == "" {
			return true // not an event, such as OpenRouter's closing [DONE]
		}
		switch ev.Type {
		case "response.created":
			id = ev.Response.ID
		case "response.output_item.done":
			output = append(output, ev.Item)
		case "response.completed":
			id = cmp.Or(ev.Response.ID, id)
		}
		if writeErr = sess.client.Write(ctx, websocket.MessageText, data); writeErr != nil {
			return false
		}
		if isTerminal(ev.Type) {
			terminal = ev.Type
			return false
		}
		return true
	})
	switch {
	case writeErr != nil:
		return writeErr
	case ctx.Err() != nil:
		return ctx.Err()
	case terminal == "":
		// An HTTP stream that ends early tells Codex by ending; a websocket
		// stays open, so it gets the error.
		message := "stream closed before response.completed"
		if readErr != nil {
			message = readErr.Error()
		}
		return sess.failMessage(ctx, http.StatusBadGateway, "", message)
	}
	if terminal == "response.completed" {
		sess.cont = continuation{responseID: id, items: append(input, output...)}
	}
	sess.end(terminal)
	return nil
}

// compact answers a compaction for a model that cannot compact, with the
// summary f asks it for.
func (sess *wsSession) compact(ctx context.Context, f forwarding, input []json.RawMessage) error {
	events, err := sess.s.summarize(ctx, f)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *upstreamError
		if errors.As(err, &ue) {
			return sess.failResponse(ctx, ue.status, ue.header, ue.body)
		}
		return sess.failMessage(ctx, http.StatusBadGateway, "", err.Error())
	}
	for _, ev := range events {
		if err := sess.client.Write(ctx, websocket.MessageText, ev); err != nil {
			return err
		}
	}
	var done struct {
		Response struct {
			ID     string            `json:"id"`
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	json.Unmarshal(events[len(events)-1], &done)
	sess.cont = continuation{responseID: done.Response.ID, items: append(input, done.Response.Output...)}
	sess.end("response.completed")
	return nil
}

// prewarm answers a message that asks for no output, which Codex sends to
// ready a socket. The next message continues from its input.
func (sess *wsSession) prewarm(ctx context.Context, model string, input []json.RawMessage) error {
	id := "resp_" + rand.Text()
	response := map[string]any{"id": id, "object": "response", "model": model, "status": "in_progress", "output": []any{}}
	if err := sess.send(ctx, map[string]any{"type": "response.created", "response": response}); err != nil {
		return err
	}
	response["status"] = "completed"
	if err := sess.send(ctx, map[string]any{"type": "response.completed", "response": response}); err != nil {
		return err
	}
	sess.cont = continuation{responseID: id, items: input}
	sess.end("response.completed")
	return nil
}

// failMessage answers the message in flight with an error event the router
// makes. Codex reads it as the HTTP error it names.
func (sess *wsSession) failMessage(ctx context.Context, status int, code, message string) error {
	return sess.fail(ctx, map[string]any{"type": "error", "status": status,
		"error": map[string]string{"code": code, "message": message}})
}

// failResponse answers the message in flight with an upstream's HTTP error.
func (sess *wsSession) failResponse(ctx context.Context, status int, header http.Header, body []byte) error {
	ev := map[string]any{"type": "error", "status": status}
	var wrapped struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &wrapped) == nil && bytes.HasPrefix(wrapped.Error, []byte("{")) {
		ev["error"] = wrapped.Error
	} else {
		ev["error"] = map[string]string{"message": strings.TrimSpace(string(body))}
	}
	headers := map[string]string{}
	for k := range header {
		headers[strings.ToLower(k)] = header.Get(k)
	}
	ev["headers"] = headers
	return sess.fail(ctx, ev)
}

func (sess *wsSession) fail(ctx context.Context, ev map[string]any) error {
	defer sess.end(fmt.Sprint("error ", ev["status"]))
	return sess.send(ctx, ev)
}

func (sess *wsSession) send(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return sess.client.Write(ctx, websocket.MessageText, data)
}

// begin counts a response in flight, so a stopping router waits for it. It
// reports false once the router is stopping.
func (sess *wsSession) begin(model string, route upstream.Route) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.draining {
		return false
	}
	if sess.current == nil {
		sess.s.inFlight.Add(1)
	}
	sess.current = &wsResponse{model: model, route: route, start: time.Now()}
	return true
}

// end logs the response in flight with how it ended, and stops counting it.
func (sess *wsSession) end(outcome string) {
	sess.endResponse(nil, outcome)
}

func (sess *wsSession) inFlightResponse() *wsResponse {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.current
}

// endResponse ends response r, or whichever is in flight when r is nil.
func (sess *wsSession) endResponse(r *wsResponse, outcome string) {
	sess.mu.Lock()
	cur, draining := sess.current, sess.draining
	if r != nil && r != cur {
		cur = nil // r has ended already
	}
	if cur != nil {
		sess.current = nil
	}
	sess.mu.Unlock()
	if cur == nil {
		return
	}
	sess.s.inFlight.Add(-1)
	slog.Info("request", "method", "WS", "model", cur.model, "route", cur.route, "outcome", outcome,
		"duration", time.Since(cur.start).Round(time.Millisecond))
	if draining {
		go sess.client.Close(websocket.StatusServiceRestart, "router restarting")
	}
}

// drain closes the session once nothing is in flight. Codex opens its next
// websocket to the router that replaces this one.
func (sess *wsSession) drain() {
	sess.mu.Lock()
	sess.draining = true
	idle := sess.current == nil
	sess.mu.Unlock()
	if idle {
		go sess.client.Close(websocket.StatusServiceRestart, "router restarting")
	}
}

// eventType is the type of a Responses event.
func eventType(data []byte) string {
	var ev struct {
		Type string `json:"type"`
	}
	json.Unmarshal(data, &ev)
	return ev.Type
}

// isTerminal reports whether an event of type t ends its response.
func isTerminal(t string) bool {
	switch t {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true
	}
	return false
}

// closeOf is the close code and reason Codex gets when OpenAI's socket ended
// with err.
func closeOf(err error) (websocket.StatusCode, string) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code, ce.Reason
	}
	return websocket.StatusGoingAway, "upstream websocket ended"
}

// readEvents calls fn with each server-sent event's data until fn returns
// false or the stream ends.
func readEvents(r io.Reader, fn func([]byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var data bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 && data.Len() > 0 {
			if !fn(bytes.Clone(data.Bytes())) {
				return nil
			}
			data.Reset()
		} else if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(v, []byte(" ")))
		}
		if err == io.EOF {
			if data.Len() > 0 {
				fn(data.Bytes())
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read events: %w", err)
		}
	}
}

// Drain begins stopping: each websocket closes once nothing is in flight on
// it, and Codex opens its next one to the router that replaces this one.
// http.Server.Shutdown does not see websockets, whose connections the router
// took over; register Drain with RegisterOnShutdown and Wait after Shutdown.
func (s *Server) Drain() {
	s.draining.Store(true)
	s.sessions.Range(func(k, _ any) bool {
		k.(*wsSession).drain()
		return true
	})
}

// Wait returns once every websocket has closed, or ctx is done.
func (s *Server) Wait(ctx context.Context) error {
	for {
		open := false
		s.sessions.Range(func(_, _ any) bool {
			open = true
			return false
		})
		if !open {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
