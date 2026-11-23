package cdp

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestCDPGracefulDegradationWhenUnavailable(t *testing.T) {
	c := NewClient("nonexistent_chrome_devtools_remote_test_99999")
	st := c.Probe(context.Background())
	if st.Available {
		t.Fatalf("expected unavailable, got %+v", st)
	}
	if _, err := c.ListTargets(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestCDPAbstractUnixSocketEndToEnd(t *testing.T) {
	sockName := fmt.Sprintf("android_agent_cdp_test_%d", 123456)
	ln, err := net.Listen("unix", "\x00"+sockName)
	if err != nil {
		t.Fatalf("listen abstract unix socket: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Browser":"Chrome/132.0","Protocol-Version":"1.3"}`)
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"PAGE1","type":"page","title":"Example","url":"https://example.com","webSocketDebuggerUrl":"ws://localhost/devtools/page/PAGE1"}]`)
	})
	mux.HandleFunc("/devtools/page/PAGE1", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(h[:])

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer does not support hijack")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		_ = rw.Flush()

		br := bufio.NewReader(conn)
		_, payload, err := wsReadFrame(br)
		if err != nil {
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(payload, &req)
		resp := fmt.Sprintf(`{"id":%d,"result":{"value":"title:%s"}}`, req.ID, req.Method)
		// Server-to-client frame (unmasked)
		frame := append([]byte{0x81, byte(len(resp))}, []byte(resp)...)
		_, _ = conn.Write(frame)
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	c := NewClient(sockName)
	st := c.Probe(context.Background())
	if !st.Available {
		t.Fatalf("expected probe available, got %+v", st)
	}
	targets, err := c.ListTargets(context.Background())
	if err != nil || len(targets) != 1 || targets[0].ID != "PAGE1" {
		t.Fatalf("unexpected targets: %+v, err=%v", targets, err)
	}
	res, err := c.SendCommand(context.Background(), "PAGE1", "Runtime.evaluate", map[string]any{"expression": "document.title"})
	if err != nil {
		t.Fatalf("SendCommand failed: %v", err)
	}
	if !strings.Contains(string(res), "title:Runtime.evaluate") {
		t.Fatalf("unexpected CDP result: %s", string(res))
	}
}
