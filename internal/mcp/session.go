package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/contracts"
)

const maxConcurrentRequests = 16

const inboundLimit = 256 << 10
const outboundLimit = 2 << 20

var errProtocol = errors.New("invalid or unavailable MCP session")

// Serve runs one initialized, bounded session on an already authenticated socket.
func Serve(parent context.Context, socket net.Conn, backend Backend, version string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer socket.Close()
	g := &guard{socket: socket, cancel: cancel, pending: make(map[jsonrpc.ID]string), cancelled: make(map[jsonrpc.ID]bool)}
	timer := time.AfterFunc(10*time.Second, func() { g.Close() })
	defer timer.Stop()
	g.initialized = func() { timer.Stop() }
	stop := context.AfterFunc(ctx, func() { g.Close() })
	defer stop()
	s := newServer(ctx, backend, version)
	session, err := s.Connect(ctx, g, nil)
	if err != nil {
		return errProtocol
	}
	defer session.Close()
	return session.Wait()
}

// guard admits requests before the SDK creates handlers. At most sixteen calls,
// including control requests, retain memory/output. Excess work closes the peer;
// notifications cannot create an unbounded handler queue.
type guard struct {
	sdk.Connection
	socket          net.Conn
	cancel          context.CancelFunc
	initialized     func()
	mu              sync.Mutex
	pending         map[jsonrpc.ID]string
	cancelled       map[jsonrpc.ID]bool
	initSent, ready bool
	writeMu         sync.Mutex
}

func (g *guard) Connect(ctx context.Context) (sdk.Connection, error) {
	reader := &frameReader{Reader: bufio.NewReaderSize(g.socket, inboundLimit+1), closer: g.socket}
	conn, err := (&sdk.IOTransport{Reader: reader, Writer: g.socket, MaxLineLength: inboundLimit}).Connect(ctx)
	g.mu.Lock()
	g.Connection = conn
	g.mu.Unlock()
	if ctx.Err() != nil {
		g.Close()
	}
	return g, err
}
func (g *guard) Close() error {
	g.cancel()
	err := g.socket.Close()
	g.mu.Lock()
	conn := g.Connection
	g.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return err
}
func (g *guard) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		msg, err := g.Connection.Read(ctx)
		if err != nil {
			g.Close()
			return nil, err
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok {
			g.Close()
			return nil, errProtocol
		}
		// This stateful stdio service implements the initialized protocol, not
		// stateless discovery/subscriptions. Let current SDK clients fall back.
		if req.IsCall() && req.Method == "server/discover" {
			g.mu.Lock()
			valid := !g.initSent && len(g.pending) == 0
			g.mu.Unlock()
			if !valid {
				g.Close()
				return nil, errProtocol
			}
			if err := g.Write(ctx, &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method unavailable"}}); err != nil {
				return nil, err
			}
			continue
		}
		g.mu.Lock()
		valid := true
		if !req.IsCall() {
			switch req.Method {
			case "notifications/initialized":
				valid = g.initSent && !g.ready
				if valid {
					g.ready = true
					g.initialized()
				}
			case "notifications/cancelled":
				valid = g.ready
				var p struct {
					RequestID any `json:"requestId"`
				}
				if json.Unmarshal(req.Params, &p) != nil {
					valid = false
				}
				id, err := jsonrpc.MakeID(p.RequestID)
				if err != nil {
					valid = false
				}
				if valid {
					if _, ok := g.pending[id]; !ok || g.cancelled[id] {
						g.mu.Unlock()
						continue
					}
					g.cancelled[id] = true
				}
			default:
				valid = false
			}
		} else {
			_, duplicate := g.pending[req.ID]
			valid = !duplicate && len(g.pending) < maxConcurrentRequests
			if req.Method == "initialize" {
				valid = valid && !g.initSent && len(g.pending) == 0
			} else {
				valid = valid && g.ready
			}
			if valid {
				g.pending[req.ID] = req.Method
			}
		}
		g.mu.Unlock()
		if !valid {
			g.Close()
			return nil, errProtocol
		}
		// Initialized has no user callback; cancellation is preempted by the SDK.
		return msg, nil
	}
}
func (g *guard) Write(ctx context.Context, msg jsonrpc.Message) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	if response, ok := msg.(*jsonrpc.Response); ok {
		if response.Error != nil {
			var wire *jsonrpc.Error
			code := int64(jsonrpc.CodeInternalError)
			if errors.As(response.Error, &wire) {
				code = wire.Code
			}
			response.Error = &jsonrpc.Error{Code: code, Message: "MCP request failed"}
		}
		g.mu.Lock()
		if g.pending[response.ID] == "initialize" && response.Error == nil {
			g.initSent = true
		}
		g.mu.Unlock()
	}
	b, err := jsonrpc.EncodeMessage(msg)
	if err != nil || len(b)+1 > outboundLimit {
		g.Close()
		return errProtocol
	}
	_ = g.socket.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = g.Connection.Write(ctx, msg)
	if err != nil {
		g.Close()
		return errProtocol
	}
	if response, ok := msg.(*jsonrpc.Response); ok {
		g.mu.Lock()
		delete(g.pending, response.ID)
		delete(g.cancelled, response.ID)
		g.mu.Unlock()
	}
	return nil
}

// frameReader rejects oversized, duplicate-key and deeply nested input before
// SDK decoding. Only one bounded line is retained and no raw errors are logged.
type frameReader struct {
	*bufio.Reader
	closer    io.Closer
	remaining []byte
}

func (r *frameReader) Read(p []byte) (int, error) {
	if len(r.remaining) == 0 {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return 0, err
		}
		if len(line) > inboundLimit || len(bytes.TrimSpace(line)) == 0 || bytes.TrimSpace(line)[0] != '{' {
			return 0, errProtocol
		}
		if _, err = contracts.JSON(bytes.NewReader(line), inboundLimit); err != nil {
			return 0, errProtocol
		}
		r.remaining = line
	}
	n := copy(p, r.remaining)
	r.remaining = r.remaining[n:]
	return n, nil
}
func (r *frameReader) Close() error { return r.closer.Close() }
