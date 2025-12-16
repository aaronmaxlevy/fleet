package http2websocketadapter

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsH2WebSocketRequest(t *testing.T) {
	tests := []struct {
		name       string
		makeReq    func() *http.Request
		wantResult bool
	}{
		{
			name: "HTTP/2 extended CONNECT with websocket protocol",
			makeReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodConnect, "/ws", nil)
				req.ProtoMajor = 2
				req.ProtoMinor = 0
				req.Header.Set(":protocol", "websocket")
				return req
			},
			wantResult: true,
		},
		{
			name: "HTTP/2 CONNECT without protocol header",
			makeReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodConnect, "/ws", nil)
				req.ProtoMajor = 2
				req.ProtoMinor = 0
				return req
			},
			wantResult: false,
		},
		{
			name: "HTTP/2 CONNECT with non-websocket protocol",
			makeReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodConnect, "/ws", nil)
				req.ProtoMajor = 2
				req.ProtoMinor = 0
				req.Header.Set(":protocol", "other")
				return req
			},
			wantResult: false,
		},
		{
			name: "HTTP/1.1 GET request",
			makeReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/ws", nil)
				req.ProtoMajor = 1
				req.ProtoMinor = 1
				return req
			},
			wantResult: false,
		},
		{
			name: "HTTP/1.1 with websocket upgrade headers",
			makeReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/ws", nil)
				req.ProtoMajor = 1
				req.ProtoMinor = 1
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
				return req
			},
			wantResult: false,
		},
		{
			name: "HTTP/2 GET request (not CONNECT)",
			makeReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/ws", nil)
				req.ProtoMajor = 2
				req.ProtoMinor = 0
				req.Header.Set(":protocol", "websocket")
				return req
			},
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.makeReq()
			got := IsH2WebSocketRequest(req)
			assert.Equal(t, tt.wantResult, got)
		})
	}
}

func TestAdaptH2WebSocketRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodConnect, "/ws", nil)
	req.ProtoMajor = 2
	req.Header.Set(":protocol", "websocket")

	AdaptH2WebSocketRequest(req)

	assert.Equal(t, http.MethodGet, req.Method)
	assert.Equal(t, "Upgrade", req.Header.Get("Connection"))
	assert.Equal(t, "websocket", req.Header.Get("Upgrade"))
}

func TestH2Stream(t *testing.T) {
	t.Run("Read", func(t *testing.T) {
		body := io.NopCloser(bytes.NewReader([]byte("hello world")))
		req := &http.Request{Body: body}
		w := httptest.NewRecorder()

		stream := NewH2Stream(req, w)

		buf := make([]byte, 5)
		n, err := stream.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, 5, n)
		assert.Equal(t, "hello", string(buf))
	})

	t.Run("Write", func(t *testing.T) {
		body := io.NopCloser(bytes.NewReader([]byte{}))
		req := &http.Request{Body: body}
		w := httptest.NewRecorder()

		stream := NewH2Stream(req, w)

		n, err := stream.Write([]byte("test data"))
		require.NoError(t, err)
		assert.Equal(t, 9, n)
		assert.Equal(t, "test data", w.Body.String())
	})

	t.Run("Close", func(t *testing.T) {
		closed := false
		body := &mockReadCloser{
			Reader: bytes.NewReader([]byte{}),
			closeFn: func() error {
				closed = true
				return nil
			},
		}
		req := &http.Request{Body: body}
		w := httptest.NewRecorder()

		stream := NewH2Stream(req, w)
		err := stream.Close()
		require.NoError(t, err)
		assert.True(t, closed)

		// Double close should be safe
		err = stream.Close()
		require.NoError(t, err)
	})
}

type mockReadCloser struct {
	io.Reader
	closeFn func() error
}

func (m *mockReadCloser) Close() error {
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

func TestH2Conn(t *testing.T) {
	body := io.NopCloser(bytes.NewReader([]byte("test")))
	req := &http.Request{Body: body}
	w := httptest.NewRecorder()

	stream := NewH2Stream(req, w)
	localAddr := NewH2Addr("localhost:8080")
	remoteAddr := NewH2Addr("192.168.1.1:54321")
	conn := NewH2Conn(stream, localAddr, remoteAddr)

	t.Run("LocalAddr", func(t *testing.T) {
		addr := conn.LocalAddr()
		assert.Equal(t, "h2", addr.Network())
		assert.Equal(t, "localhost:8080", addr.String())
	})

	t.Run("RemoteAddr", func(t *testing.T) {
		addr := conn.RemoteAddr()
		assert.Equal(t, "h2", addr.Network())
		assert.Equal(t, "192.168.1.1:54321", addr.String())
	})

	t.Run("SetDeadline no-ops", func(t *testing.T) {
		assert.NoError(t, conn.SetDeadline(time.Time{}))
		assert.NoError(t, conn.SetReadDeadline(time.Time{}))
		assert.NoError(t, conn.SetWriteDeadline(time.Time{}))
	})
}

func TestH2HijackableResponseWriter(t *testing.T) {
	t.Run("Hijack returns conn and bufio", func(t *testing.T) {
		body := io.NopCloser(bytes.NewReader([]byte("request data")))
		req := httptest.NewRequest(http.MethodConnect, "/ws", body)
		req.ProtoMajor = 2
		req.RemoteAddr = "192.168.1.1:54321"
		w := httptest.NewRecorder()

		hijackable := NewH2HijackableResponseWriter(w, req)

		conn, brw, err := hijackable.Hijack()
		require.NoError(t, err)
		require.NotNil(t, conn)
		require.NotNil(t, brw)

		// Verify we can read through the connection
		buf := make([]byte, 12)
		n, err := conn.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, 12, n)
		assert.Equal(t, "request data", string(buf))
	})

	t.Run("Multiple hijacks return error", func(t *testing.T) {
		body := io.NopCloser(bytes.NewReader([]byte{}))
		req := httptest.NewRequest(http.MethodConnect, "/ws", body)
		req.ProtoMajor = 2
		w := httptest.NewRecorder()

		hijackable := NewH2HijackableResponseWriter(w, req)

		// First hijack succeeds
		_, _, err := hijackable.Hijack()
		require.NoError(t, err)

		// Second hijack fails
		_, _, err = hijackable.Hijack()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already hijacked")
	})
}

func TestH2ResponseWriterAdapter(t *testing.T) {
	t.Run("Converts 101 to 200", func(t *testing.T) {
		body := io.NopCloser(bytes.NewReader([]byte{}))
		req := httptest.NewRequest(http.MethodConnect, "/ws", body)
		req.ProtoMajor = 2
		w := httptest.NewRecorder()
		adapter := NewH2ResponseWriterAdapter(w, req)

		adapter.WriteHeader(http.StatusSwitchingProtocols)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("Passes through other status codes", func(t *testing.T) {
		body := io.NopCloser(bytes.NewReader([]byte{}))
		req := httptest.NewRequest(http.MethodConnect, "/ws", body)
		req.ProtoMajor = 2
		w := httptest.NewRecorder()
		adapter := NewH2ResponseWriterAdapter(w, req)

		adapter.WriteHeader(http.StatusNotFound)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Write triggers implicit 200 on first call", func(t *testing.T) {
		body := io.NopCloser(bytes.NewReader([]byte{}))
		req := httptest.NewRequest(http.MethodConnect, "/ws", body)
		req.ProtoMajor = 2
		w := httptest.NewRecorder()
		adapter := NewH2ResponseWriterAdapter(w, req)

		_, err := adapter.Write([]byte("data"))
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "data", w.Body.String())
	})
}

func TestMiddleware(t *testing.T) {
	t.Run("Passes through non-H2 WebSocket requests", func(t *testing.T) {
		handlerCalled := false
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			assert.Equal(t, http.MethodGet, r.Method)
			// Should NOT have modified headers for non-H2 request
			assert.NotEqual(t, "Upgrade", r.Header.Get("Connection"))
		})

		mw := Middleware(handler)

		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.ProtoMajor = 1
		req.ProtoMinor = 1
		w := httptest.NewRecorder()

		mw.ServeHTTP(w, req)

		assert.True(t, handlerCalled)
	})

	t.Run("Adapts HTTP/2 WebSocket requests", func(t *testing.T) {
		handlerCalled := false
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			// Method should be changed to GET
			assert.Equal(t, http.MethodGet, r.Method)
			// Should have WebSocket upgrade headers
			assert.Equal(t, "Upgrade", r.Header.Get("Connection"))
			assert.Equal(t, "websocket", r.Header.Get("Upgrade"))

			// ResponseWriter should be hijackable
			_, ok := w.(http.Hijacker)
			assert.True(t, ok, "ResponseWriter should implement http.Hijacker")
		})

		mw := Middleware(handler)

		body := io.NopCloser(bytes.NewReader([]byte{}))
		req := httptest.NewRequest(http.MethodConnect, "/ws", body)
		req.ProtoMajor = 2
		req.ProtoMinor = 0
		req.Header.Set(":protocol", "websocket")
		w := httptest.NewRecorder()

		mw.ServeHTTP(w, req)

		assert.True(t, handlerCalled)
	})
}
