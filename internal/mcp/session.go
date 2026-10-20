package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

const maxConcurrentRequests = 16

var errProtocol = errors.New("invalid or unavailable MCP session")

// Serve owns one authenticated session. Request failures never end its transport.
func Serve(parent context.Context, socket net.Conn, backend Backend, version string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	g := &guard{socket: socket, ctx: ctx, cancel: cancel, reader: bufio.NewReader(socket), pending: make(map[jsonrpc.ID]pendingRequest)}
	defer g.Close()
	stop := context.AfterFunc(ctx, func() { g.Close() })
	defer stop()
	session, err := newServer(ctx, backend, version).Connect(ctx, g, nil)
	if err != nil {
		return errProtocol
	}
	defer session.Close()
	return session.Wait()
}

type pendingRequest struct {
	method    string
	wireID    json.RawMessage
	cancelled bool
}

// guard is an SDK transport and connection. Decode failures are handled inside
// Read: returning them to the SDK would cancel every in-flight call and close it.
// Internal string IDs preserve exact wire integer IDs, including values > 2^53.
// Writes and request retirement share one ordering, including direct rejections.
type guard struct {
	socket                        net.Conn
	ctx                           context.Context
	cancel                        context.CancelFunc
	reader                        *bufio.Reader
	closeOnce                     sync.Once
	mu                            sync.Mutex
	pending                       map[jsonrpc.ID]pendingRequest
	initSent, initializing, ready bool
	writeMu                       sync.Mutex
}

func (g *guard) Connect(context.Context) (sdk.Connection, error) { return g, nil }
func (g *guard) SessionID() string                               { return "" }
func (g *guard) Close() error {
	g.closeOnce.Do(func() { g.cancel(); _ = g.socket.Close() })
	return nil
}

// requestID returns a private SDK key and the original wire ID. MCP uses string
// or integer request IDs; keeping their namespaces distinct prevents collisions.
func requestID(raw json.RawMessage) (jsonrpc.ID, string, bool) {
	raw = bytes.TrimSpace(raw)
	var value string
	key := ""
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &value) == nil {
		key = "string:" + value
	} else {
		n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
		if err != nil {
			return jsonrpc.ID{}, "", false
		}
		key = "number:" + strconv.FormatInt(n, 10)
	}
	id, err := jsonrpc.MakeID(key)
	return id, key, err == nil
}

func (g *guard) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := g.reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(line) {
			if err := g.reject(nil, jsonrpc.CodeParseError); err != nil {
				return nil, err
			}
			continue
		}
		if contracts.JSONMessage(line) != nil {
			if err := g.reject(nil, jsonrpc.CodeInvalidRequest); err != nil {
				return nil, err
			}
			continue
		}
		var wire struct {
			Version string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  *string         `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &wire) != nil || wire.Version != "2.0" {
			if err := g.reject(nil, jsonrpc.CodeInvalidRequest); err != nil {
				return nil, err
			}
			continue
		}
		if wire.Method == nil {
			// No server-originated calls are exposed. Unsolicited responses must
			// not reach SDK decoding or affect the client's active requests.
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(line, &fields)
			if fields["result"] == nil && fields["error"] == nil {
				if err := g.reject(nil, jsonrpc.CodeInvalidRequest); err != nil {
					return nil, err
				}
			}
			continue
		}
		var id jsonrpc.ID
		if len(wire.ID) > 0 {
			var valid bool
			id, _, valid = requestID(wire.ID)
			if !valid {
				if err := g.reject(nil, jsonrpc.CodeInvalidRequest); err != nil {
					return nil, err
				}
				continue
			}
		}
		req := &jsonrpc.Request{ID: id, Method: *wire.Method, Params: wire.Params}
		if req.Method == "" {
			if req.IsCall() {
				if err := g.reject(wire.ID, jsonrpc.CodeInvalidRequest); err != nil {
					return nil, err
				}
			}
			continue
		}
		g.mu.Lock()
		if !req.IsCall() {
			valid := false
			switch req.Method {
			case "notifications/initialized":
				var params map[string]json.RawMessage
				valid = g.initSent && !g.ready && (len(req.Params) == 0 || json.Unmarshal(req.Params, &params) == nil)
				if valid {
					g.ready = true
				}
			case "notifications/cancelled":
				var params struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if g.ready && json.Unmarshal(req.Params, &params) == nil {
					cancelID, key, ok := requestID(params.RequestID)
					p, exists := g.pending[cancelID]
					valid = ok && exists && !p.cancelled
					if valid {
						p.cancelled = true
						g.pending[cancelID] = p
						req.Params, _ = json.Marshal(map[string]string{"requestId": key})
					}
				}
			}
			g.mu.Unlock()
			if valid {
				return req, nil
			}
			continue // Notifications never receive error responses.
		}
		if _, duplicate := g.pending[id]; duplicate {
			g.mu.Unlock()
			continue // Preserve the original call and its one response.
		}
		code := int64(0)
		switch {
		case req.Method == "server/discover":
			code = jsonrpc.CodeMethodNotFound
		case req.Method == "initialize" && (g.initSent || g.initializing):
			code = jsonrpc.CodeInvalidRequest
		case req.Method != "initialize" && req.Method != "ping" && !g.ready:
			code = jsonrpc.CodeInvalidRequest
		case len(g.pending) >= maxConcurrentRequests:
			code = -32000 // Server-defined busy error for control requests.
		}
		if code == 0 {
			g.pending[id] = pendingRequest{method: req.Method, wireID: bytes.Clone(wire.ID)}
			if req.Method == "initialize" {
				g.initializing = true
			}
		}
		g.mu.Unlock()
		if code != 0 {
			if code == -32000 && req.Method == "tools/call" {
				body, _ := json.Marshal(failureResult(database.Fail(contracts.ResourceLimit, "session request capacity exceeded", true)))
				err = g.send(wire.ID, body, nil)
			} else {
				err = g.reject(wire.ID, code)
			}
			if err != nil {
				return nil, err
			}
			continue
		}
		return req, nil
	}
}

func (g *guard) reject(id json.RawMessage, code int64) error {
	return g.send(id, nil, &jsonrpc.Error{Code: code, Message: "MCP request failed"})
}

func responseBytes(id, result json.RawMessage, failure *jsonrpc.Error) ([]byte, error) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	if failure != nil {
		result = nil
	} else if len(result) == 0 {
		result = json.RawMessage("null")
	}
	return json.Marshal(struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *jsonrpc.Error  `json:"error,omitempty"`
	}{"2.0", id, result, failure})
}

func (g *guard) send(id, result json.RawMessage, failure *jsonrpc.Error) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	b, err := responseBytes(id, result, failure)
	if err != nil {
		b, err = responseBytes(id, nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "MCP request failed"})
	}
	if err != nil {
		return err
	}
	return g.write(b)
}

func (g *guard) Write(_ context.Context, msg jsonrpc.Message) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	if response, ok := msg.(*jsonrpc.Response); ok {
		g.mu.Lock()
		p, exists := g.pending[response.ID]
		g.mu.Unlock()
		if !exists {
			return nil
		}
		var failure *jsonrpc.Error
		if response.Error != nil {
			code := int64(jsonrpc.CodeInternalError)
			var rpc *jsonrpc.Error
			if errors.As(response.Error, &rpc) {
				code = rpc.Code
			}
			failure = &jsonrpc.Error{Code: code, Message: "MCP request failed"}
		}
		b, err := responseBytes(p.wireID, response.Result, failure)
		if err != nil {
			failure = &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "MCP request failed"}
			b, err = responseBytes(p.wireID, nil, failure)
		}
		if err != nil {
			return err
		}
		g.mu.Lock()
		if p.method == "initialize" {
			g.initializing = false
			g.initSent = failure == nil
		}
		delete(g.pending, response.ID)
		g.mu.Unlock()
		return g.write(b)
	}
	b, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return nil
	} // Invalid server notifications cannot poison peers.
	return g.write(b)
}

func (g *guard) write(b []byte) error {
	if err := g.ctx.Err(); err != nil {
		return err
	}
	_, err := io.Copy(g.socket, bytes.NewReader(append(b, '\n')))
	if err != nil {
		g.Close()
	}
	return err
}
