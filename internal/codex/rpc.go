// Package codex inspects and controls explicitly selected native Codex runtimes.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const rpcTimeout = 10 * time.Second

type RPC struct {
	conn *websocket.Conn
	mu   sync.Mutex
	next int64
	Info ServerInfo
}

type ServerInfo struct {
	UserAgent  string `json:"userAgent"`
	CodexHome  string `json:"codexHome"`
	PlatformOS string `json:"platformOs"`
}

type rpcError struct {
	Code int `json:"code"`
}

// DialRPC connects only to an explicitly selected, same-user private Unix socket.
// No TCP fallback, ambient proxy, daemon startup or account refresh occurs.
func DialRPC(ctx context.Context, socket, version string) (*RPC, error) {
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Codex endpoint is not a Unix socket")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Codex socket must be private and owned by the current user")
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Codex socket redirects are refused") }}
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	transport.CloseIdleConnections()
	if err != nil {
		return nil, errors.New("cannot connect to Codex Unix RPC endpoint")
	}
	conn.SetReadLimit(8 << 20)
	rpc := &RPC{conn: conn}
	params := map[string]any{"clientInfo": map[string]string{"name": "verso", "title": "Verso", "version": version}, "capabilities": map[string]bool{"experimentalApi": true}}
	if err := rpc.Call(ctx, "initialize", params, &rpc.Info); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	if err := rpc.notify(ctx, "initialized"); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	return rpc, nil
}

func (r *RPC) Close() error { return r.conn.CloseNow() }

func (r *RPC) notify(ctx context.Context, method string) error {
	raw, _ := json.Marshal(map[string]any{"method": method})
	return r.conn.Write(ctx, websocket.MessageText, raw)
}

// Call ignores unrelated notifications and never includes server payloads in errors.
// The client is deliberately serial: background streaming is not a Verso feature.
func (r *RPC) Call(ctx context.Context, method string, params any, result any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	r.next++
	id := r.next
	raw, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("encode %s request", method)
	}
	if err := r.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("send %s request: %w", method, ctxError(ctx, err))
	}
	for {
		_, raw, err := r.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read %s response: %w", method, ctxError(ctx, err))
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			return fmt.Errorf("invalid %s RPC response", method)
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 {
				reply, _ := json.Marshal(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "Verso does not handle server requests"}})
				if err := r.conn.Write(ctx, websocket.MessageText, reply); err != nil {
					return errors.New("cannot decline unsolicited Codex request")
				}
			}
			continue
		}
		var responseID int64
		if json.Unmarshal(msg.ID, &responseID) != nil || responseID != id {
			continue
		}
		if msg.Error != nil {
			return fmt.Errorf("Codex %s RPC error %d", method, msg.Error.Code)
		}
		if len(msg.Result) == 0 {
			return fmt.Errorf("Codex %s response has no result", method)
		}
		if result == nil {
			return nil
		}
		if json.Unmarshal(msg.Result, result) != nil {
			return fmt.Errorf("invalid Codex %s result shape", method)
		}
		return nil
	}
}

func ctxError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// WebSocket errors can contain peer-provided close reasons. Don't echo them.
	_ = err
	return errors.New("RPC transport closed or failed")
}
