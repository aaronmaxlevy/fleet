package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/fleetdm/fleet/v4/pkg/fleethttp"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/service/http2websocketadapter"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveQueryWithContext(t *testing.T) {
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/latest/fleet/queries/run_by_identifiers":
			resp := createDistributedQueryCampaignResponse{
				Campaign: &fleet.DistributedQueryCampaign{
					UpdateCreateTimestamps: fleet.UpdateCreateTimestamps{
						CreateTimestamp: fleet.CreateTimestamp{CreatedAt: time.Now()},
						UpdateTimestamp: fleet.UpdateTimestamp{UpdatedAt: time.Now()},
					},
					Metrics: fleet.TargetMetrics{
						TotalHosts:           1,
						OnlineHosts:          1,
						OfflineHosts:         0,
						MissingInActionHosts: 0,
						NewHosts:             0,
					},
					ID:      99,
					QueryID: 42,
					Status:  0,
					UserID:  23,
				},
			}
			err := json.NewEncoder(w).Encode(resp)
			assert.NoError(t, err)
		case "/api/latest/fleet/results/websocket":
			ws, _ := upgrader.Upgrade(w, r, nil)
			defer ws.Close()

			for {
				time.Sleep(1 * time.Second)
				mt, message, _ := ws.ReadMessage()
				if string(message) == `{"type":"auth","data":{"token":"1234"}}` {
					return
				}
				if string(message) == `{"type":"select_campaign","data":{"campaign_id":99}}` {
					return
				}

				result := struct {
					Type string                       `json:"type"`
					Data fleet.DistributedQueryResult `json:"data"`
				}{
					Type: "result",
					Data: fleet.DistributedQueryResult{
						DistributedQueryCampaignID: 99,
						Host: fleet.ResultHostData{
							ID:       23,
							Hostname: "somehostaaa",
						},
						Rows: []map[string]string{
							{
								"col1": "aaa",
								"col2": "bbb",
							},
						},
						Error: nil,
					},
				}
				b, err := json.Marshal(result)
				assert.NoError(t, err)
				_ = ws.WriteMessage(mt, b)
			}
		}
	}))
	defer ts.Close()

	baseURL, err := url.Parse(ts.URL)
	require.NoError(t, err)
	client := &Client{
		baseClient: &baseClient{
			baseURL:            baseURL,
			http:               fleethttp.NewClient(),
			insecureSkipVerify: false,
			urlPrefix:          "",
		},
		token:        "1234",
		outputWriter: nil,
	}
	ctx, cancelFunc := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFunc()

	res, err := client.LiveQueryWithContext(ctx, "select 1;", nil, nil, []string{"host1"})
	require.NoError(t, err)

	gotResults := false
	go func() {
		for {
			select {
			case <-res.Results():
				gotResults = true
				cancelFunc()
			case err := <-res.Errors():
				require.NoError(t, err)
			case <-ctx.Done():
				return
			}
		}
	}()
	<-ctx.Done()
	assert.True(t, gotResults)
}

// TestHTTP2WebSocketAdapter tests that the http2websocketadapter middleware
// correctly transforms HTTP/2 extended CONNECT requests to work with
// gorilla/websocket handlers that expect HTTP/1.1-style upgrade requests.
func TestHTTP2WebSocketAdapter(t *testing.T) {
	t.Run("adapts HTTP/2 extended CONNECT to work with websocket upgrader", func(t *testing.T) {
		var hijackedConn bool
		var gotWebSocketConn bool
		var receivedMessage string

		// Create a handler that uses gorilla/websocket upgrader
		wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify the request has been adapted to look like HTTP/1.1 WebSocket
			assert.Equal(t, http.MethodGet, r.Method, "Method should be GET after adaptation")
			assert.Equal(t, "Upgrade", r.Header.Get("Connection"), "Should have Connection: Upgrade header")
			assert.Equal(t, "websocket", r.Header.Get("Upgrade"), "Should have Upgrade: websocket header")

			// Verify the ResponseWriter is hijackable
			hijacker, ok := w.(http.Hijacker)
			require.True(t, ok, "ResponseWriter should implement http.Hijacker")

			// Test that Hijack works (this is what gorilla/websocket does internally)
			conn, brw, err := hijacker.Hijack()
			require.NoError(t, err, "Hijack should succeed")
			require.NotNil(t, conn, "Connection should not be nil")
			require.NotNil(t, brw, "Buffered reader/writer should not be nil")
			hijackedConn = true

			// Read from the connection (simulating what happens after websocket handshake)
			line, err := brw.ReadString('\n')
			if err == nil {
				receivedMessage = line
				gotWebSocketConn = true
			}

			// Write a response back
			_, _ = brw.WriteString("Hello from server\n")
			_ = brw.Flush()
		})

		// Wrap with the HTTP/2 WebSocket adapter middleware
		handler := http2websocketadapter.Middleware(wsHandler)

		// Create a mock HTTP/2 extended CONNECT request
		// In real HTTP/2, req.Body is the bidirectional stream
		requestData := "Hello from client\n"
		responseBuffer := &bytes.Buffer{}
		body := io.NopCloser(bytes.NewReader([]byte(requestData)))

		req := httptest.NewRequest(http.MethodConnect, "/api/latest/fleet/results/websocket", body)
		req.ProtoMajor = 2
		req.ProtoMinor = 0
		req.Header.Set(":protocol", "websocket")
		req.RemoteAddr = "192.168.1.100:12345"

		// Create a custom ResponseWriter that captures writes
		w := &mockH2ResponseWriter{
			header:     make(http.Header),
			body:       responseBuffer,
			statusCode: 0,
		}

		handler.ServeHTTP(w, req)

		assert.True(t, hijackedConn, "Connection should have been hijacked")
		assert.True(t, gotWebSocketConn, "Should have successfully read from WebSocket connection")
		assert.Equal(t, requestData, receivedMessage, "Should have received the message from client")
	})

	t.Run("passes through non-HTTP/2 requests unchanged", func(t *testing.T) {
		handlerCalled := false
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			// For HTTP/1.1 requests, the method should NOT be modified
			assert.Equal(t, http.MethodGet, r.Method)
			// Should NOT have injected headers
			assert.Empty(t, r.Header.Get("Connection"))
			assert.Empty(t, r.Header.Get("Upgrade"))
		})

		mw := http2websocketadapter.Middleware(handler)

		req := httptest.NewRequest(http.MethodGet, "/api/latest/fleet/results/websocket", nil)
		req.ProtoMajor = 1
		req.ProtoMinor = 1
		w := httptest.NewRecorder()

		mw.ServeHTTP(w, req)

		assert.True(t, handlerCalled, "Handler should have been called")
	})

	t.Run("converts 101 status to 200 for HTTP/2", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate what gorilla/websocket does - write 101 Switching Protocols
			w.WriteHeader(http.StatusSwitchingProtocols)
		})

		mw := http2websocketadapter.Middleware(handler)

		body := io.NopCloser(bytes.NewReader([]byte{}))
		req := httptest.NewRequest(http.MethodConnect, "/ws", body)
		req.ProtoMajor = 2
		req.Header.Set(":protocol", "websocket")
		w := httptest.NewRecorder()

		mw.ServeHTTP(w, req)

		// For HTTP/2, 101 should be converted to 200
		assert.Equal(t, http.StatusOK, w.Code, "Status should be 200 OK for HTTP/2")
	})
}

// mockH2ResponseWriter simulates an HTTP/2 ResponseWriter that supports
// the patterns needed for the adapter test.
type mockH2ResponseWriter struct {
	header     http.Header
	body       *bytes.Buffer
	statusCode int
}

func (w *mockH2ResponseWriter) Header() http.Header {
	return w.header
}

func (w *mockH2ResponseWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

func (w *mockH2ResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *mockH2ResponseWriter) Flush() {
	// no-op for mock
}

// Hijack implements http.Hijacker - but the adapter wraps this, so this
// shouldn't normally be called directly. We implement it to satisfy the
// interface in case it's checked.
func (w *mockH2ResponseWriter) Hijack() (conn interface{}, rw *bufio.ReadWriter, err error) {
	return nil, nil, nil
}
