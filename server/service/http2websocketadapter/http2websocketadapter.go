// Package http2websocketadapter provides an adapter for handling WebSocket connections
// over HTTP/2 extended CONNECT (RFC 8441).
//
// When GODEBUG=http2xconnect=1 is set, Go's HTTP/2 server supports RFC 8441,
// allowing WebSocket connections over HTTP/2 streams. This package provides
// middleware that detects such requests and handles them appropriately.
//
// For HTTP/2 extended CONNECT WebSocket requests:
// - Method is CONNECT (not GET)
// - The ":protocol" pseudo-header is set to "websocket"
// - There are no Connection/Upgrade headers
// - Response is 200 OK (not 101 Switching Protocols)
// - The stream is bidirectional: req.Body for reading, ResponseWriter for writing
package http2websocketadapter

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// IsH2WebSocketRequest returns true if the request is an HTTP/2 extended CONNECT
// request for WebSocket (RFC 8441).
func IsH2WebSocketRequest(r *http.Request) bool {
	return r.ProtoMajor == 2 &&
		r.Method == http.MethodConnect &&
		r.Header.Get(":protocol") == "websocket"
}

// H2Stream wraps an HTTP/2 stream as an io.ReadWriteCloser suitable for WebSocket.
// It reads from the request body and writes to the response writer.
type H2Stream struct {
	r     io.ReadCloser
	w     http.ResponseWriter
	flush func() error

	mu     sync.Mutex
	closed bool
}

// NewH2Stream creates a new H2Stream from an HTTP/2 request.
// Call this after writing the 200 OK response status.
func NewH2Stream(r *http.Request, w http.ResponseWriter) *H2Stream {
	rc := http.NewResponseController(w)
	return &H2Stream{
		r:     r.Body,
		w:     w,
		flush: rc.Flush,
	}
}

func (s *H2Stream) Read(p []byte) (int, error) {
	return s.r.Read(p)
}

func (s *H2Stream) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil {
		return n, err
	}
	if err := s.flush(); err != nil {
		return n, err
	}
	return n, nil
}

func (s *H2Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	// Flush before closing to ensure all data is sent
	_ = s.flush()
	return s.r.Close()
}

// H2Conn wraps an H2Stream to implement net.Conn for use with gorilla/websocket.
// Note: Some net.Conn methods are no-ops since HTTP/2 streams don't have the
// same semantics as raw TCP connections.
type H2Conn struct {
	*H2Stream
	localAddr  net.Addr
	remoteAddr net.Addr
}

// NewH2Conn creates a net.Conn wrapper around an H2Stream.
func NewH2Conn(stream *H2Stream, localAddr, remoteAddr net.Addr) *H2Conn {
	return &H2Conn{
		H2Stream:   stream,
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
	}
}

func (c *H2Conn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *H2Conn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline is a no-op for HTTP/2 streams.
// HTTP/2 stream deadlines are managed differently.
func (c *H2Conn) SetDeadline(t time.Time) error {
	return nil
}

// SetReadDeadline is a no-op for HTTP/2 streams.
func (c *H2Conn) SetReadDeadline(t time.Time) error {
	return nil
}

// SetWriteDeadline is a no-op for HTTP/2 streams.
func (c *H2Conn) SetWriteDeadline(t time.Time) error {
	return nil
}

// h2Addr is a simple net.Addr implementation for HTTP/2 connections.
type h2Addr struct {
	addr string
}

func (a h2Addr) Network() string { return "h2" }
func (a h2Addr) String() string  { return a.addr }

// NewH2Addr creates a net.Addr for an HTTP/2 connection.
func NewH2Addr(addr string) net.Addr {
	return h2Addr{addr: addr}
}

// H2HijackableResponseWriter wraps an http.ResponseWriter to provide a fake
// Hijacker implementation that returns an H2Conn for HTTP/2 WebSocket streams.
// This allows gorilla/websocket to work with HTTP/2 extended CONNECT requests.
type H2HijackableResponseWriter struct {
	http.ResponseWriter
	conn       *H2Conn
	hijackOnce sync.Once
	hijacked   bool
}

// NewH2HijackableResponseWriter creates a ResponseWriter wrapper that provides
// hijacking support for HTTP/2 WebSocket streams.
func NewH2HijackableResponseWriter(w http.ResponseWriter, r *http.Request) *H2HijackableResponseWriter {
	stream := NewH2Stream(r, w)
	localAddr := NewH2Addr(r.Host)
	remoteAddr := NewH2Addr(r.RemoteAddr)
	conn := NewH2Conn(stream, localAddr, remoteAddr)

	return &H2HijackableResponseWriter{
		ResponseWriter: w,
		conn:           conn,
	}
}

// Hijack implements http.Hijacker for HTTP/2 streams.
// Unlike HTTP/1.1 hijacking, this doesn't actually hijack a TCP connection
// but returns a net.Conn wrapper around the HTTP/2 bidirectional stream.
func (w *H2HijackableResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	alreadyHijacked := true
	w.hijackOnce.Do(func() {
		alreadyHijacked = false
		w.hijacked = true
	})
	if alreadyHijacked {
		return nil, nil, errors.New("connection already hijacked")
	}

	// Create buffered reader/writer for the connection
	br := bufio.NewReader(w.conn)
	bw := bufio.NewWriter(w.conn)
	brw := bufio.NewReadWriter(br, bw)

	return w.conn, brw, nil
}

// Flush implements http.Flusher
func (w *H2HijackableResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// AdaptH2WebSocketRequest modifies an HTTP/2 extended CONNECT WebSocket request
// to look like an HTTP/1.1 WebSocket upgrade request. This allows libraries like
// gorilla/websocket that check for specific headers to work with HTTP/2.
//
// The modifications are:
// - Method: CONNECT -> GET
// - Add "Connection: Upgrade" header
// - Add "Upgrade: websocket" header
//
// Note: This modifies the request in place.
func AdaptH2WebSocketRequest(r *http.Request) {
	r.Method = http.MethodGet
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
}

// H2ResponseWriterAdapter wraps the ResponseWriter to intercept the 101 status
// that gorilla/websocket writes and convert it to 200 for HTTP/2.
type H2ResponseWriterAdapter struct {
	*H2HijackableResponseWriter
	headerWritten bool
}

// NewH2ResponseWriterAdapter creates a new adapter that handles both hijacking
// and status code translation for HTTP/2 WebSocket connections.
func NewH2ResponseWriterAdapter(w http.ResponseWriter, r *http.Request) *H2ResponseWriterAdapter {
	return &H2ResponseWriterAdapter{
		H2HijackableResponseWriter: NewH2HijackableResponseWriter(w, r),
	}
}

// WriteHeader intercepts the 101 Switching Protocols response and converts it
// to 200 OK for HTTP/2, as required by RFC 8441.
func (w *H2ResponseWriterAdapter) WriteHeader(statusCode int) {
	if w.headerWritten {
		return
	}
	w.headerWritten = true

	// For HTTP/2, convert 101 Switching Protocols to 200 OK
	if statusCode == http.StatusSwitchingProtocols {
		statusCode = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(statusCode)

	// Flush immediately to complete the handshake
	w.Flush()
}

// Write ensures headers are written before any body content.
func (w *H2ResponseWriterAdapter) Write(b []byte) (int, error) {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Middleware returns an HTTP middleware that adapts HTTP/2 extended CONNECT
// WebSocket requests to work with HTTP/1.1-based WebSocket handlers like
// gorilla/websocket and sockjs-go.
//
// Usage:
//
//	handler := http2websocketadapter.Middleware(sockjsHandler)
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsH2WebSocketRequest(r) {
			// Not an HTTP/2 WebSocket request, pass through unchanged
			next.ServeHTTP(w, r)
			return
		}

		// Adapt the request to look like HTTP/1.1 WebSocket upgrade
		AdaptH2WebSocketRequest(r)

		// Wrap the ResponseWriter to provide hijacking and status translation
		adapter := NewH2ResponseWriterAdapter(w, r)

		next.ServeHTTP(adapter, r)
	})
}
