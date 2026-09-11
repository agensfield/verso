package codex

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func rpcServer(t *testing.T, fn func(context.Context, *websocket.Conn, map[string]json.RawMessage)) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	s := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, raw, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req map[string]json.RawMessage
			if json.Unmarshal(raw, &req) != nil {
				return
			}
			var method string
			_ = json.Unmarshal(req["method"], &method)
			if method == "initialize" {
				sendRPC(r.Context(), c, req["id"], map[string]string{"userAgent": "codex/0.154.0"})
				continue
			}
			if method == "initialized" {
				continue
			}
			fn(r.Context(), c, req)
		}
	})}
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Close(); _ = l.Close() })
	return socket
}
func sendRPC(ctx context.Context, c *websocket.Conn, id json.RawMessage, value any) {
	raw, _ := json.Marshal(map[string]any{"id": id, "result": value})
	_ = c.Write(ctx, websocket.MessageText, raw)
}
func TestRPCLocalHandshakeAndNotifications(t *testing.T) {
	socket := rpcServer(t, func(ctx context.Context, c *websocket.Conn, req map[string]json.RawMessage) {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"method":"unrelated","params":{"text":"private transcript"}}`))
		sendRPC(ctx, c, req["id"], map[string]string{"value": "ok"})
	})
	c, err := DialRPC(context.Background(), socket, "0.1.0-alpha.1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var got struct {
		Value string `json:"value"`
	}
	if err := c.Call(context.Background(), "account/read", map[string]bool{"refreshToken": false}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Value != "ok" {
		t.Fatal(got)
	}
}
func TestRPCDoesNotEchoServerSecrets(t *testing.T) {
	socket := rpcServer(t, func(ctx context.Context, c *websocket.Conn, req map[string]json.RawMessage) {
		raw, _ := json.Marshal(map[string]any{"id": req["id"], "error": map[string]any{"code": -32600, "message": "secret_token_goes_here"}})
		_ = c.Write(ctx, websocket.MessageText, raw)
	})
	c, err := DialRPC(context.Background(), socket, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Call(context.Background(), "account/read", nil, nil)
	if err == nil || strings.Contains(err.Error(), "secret_token") {
		t.Fatalf("error=%v", err)
	}
}
func TestRPCRefusesNonSocketAndSymlink(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DialRPC(context.Background(), f, "test"); err == nil {
		t.Fatal("accepted regular file")
	}
	socket := rpcServer(t, func(context.Context, *websocket.Conn, map[string]json.RawMessage) {})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	if _, err := DialRPC(context.Background(), link, "test"); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Chmod(socket, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := DialRPC(context.Background(), socket, "test"); err == nil {
		t.Fatal("accepted public socket")
	}
}
func TestRPCCancellation(t *testing.T) {
	socket := rpcServer(t, func(context.Context, *websocket.Conn, map[string]json.RawMessage) {})
	c, err := DialRPC(context.Background(), socket, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "account/read", nil, nil); err == nil {
		t.Fatal("missing timeout")
	}
}
