// Package mcp implements bounded MCP sessions and the stdio relay.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Backend keeps state leases and credentials in the service, outside protocol handling.
type Backend interface {
	Work(context.Context, string, func(context.Context, database.Driver, database.Access) error) error
	Connections(context.Context, func([]Connection) error) error
}

// Connection contains only the explicitly public profile fields.
type Connection struct {
	Alias    string       `json:"alias"`
	Driver   string       `json:"driver"`
	Database string       `json:"database"`
	Scope    config.Scope `json:"scope"`
}

type arguments struct {
	Connection string            `json:"connection"`
	Schema     string            `json:"schema"`
	Table      string            `json:"table"`
	Cursor     string            `json:"cursor"`
	PageSize   int               `json:"page_size"`
	SQL        string            `json:"sql"`
	Parameters []json.RawMessage `json:"parameters"`
	RowLimit   int               `json:"row_limit"`
}

func newServer(ctx context.Context, backend Backend, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "data-mate", Version: version}, &sdk.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, name := range []string{"list_connections", "list_tables", "describe_table", "query"} {
		input, _ := contracts.Schemas.ReadFile("schemas/" + name + ".input.json")
		output, _ := contracts.Schemas.ReadFile("schemas/" + name + ".output.json")
		s.AddTool(&sdk.Tool{Name: name, InputSchema: json.RawMessage(input), OutputSchema: json.RawMessage(output), Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, func(callCtx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			callCtx, cancel := context.WithCancel(callCtx)
			defer cancel()
			stop := context.AfterFunc(ctx, cancel)
			defer stop()
			raw := req.Params.Arguments
			if len(raw) == 0 {
				raw = json.RawMessage(`{}`)
			}
			if contracts.Validate(name+".input", raw) != nil {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid tool arguments"}
			}
			var args arguments
			if json.Unmarshal(raw, &args) != nil {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid tool arguments"}
			}
			var result *sdk.CallToolResult
			prepare := func(value any, limit int) error {
				var err error
				result, err = boundedResult(value, limit)
				return err
			}
			var err error
			if name == "list_connections" {
				err = backend.Connections(callCtx, func(items []Connection) error {
					return prepare(struct {
						Connections []Connection `json:"connections"`
					}{items}, 1<<20)
				})
			} else {
				err = backend.Work(callCtx, args.Connection, func(ctx context.Context, d database.Driver, a database.Access) error {
					var value any
					var err error
					switch name {
					case "list_tables":
						value, err = d.ListTables(ctx, a, database.PageRequest{Schema: args.Schema, Cursor: args.Cursor, PageSize: args.PageSize})
					case "describe_table":
						value, err = d.DescribeTable(ctx, a, config.Table{Schema: args.Schema, Name: args.Table})
					case "query":
						value, err = d.Query(ctx, a, database.QueryRequest{SQL: args.SQL, Parameters: args.Parameters, RowLimit: args.RowLimit})
					}
					if err != nil {
						return err
					}
					return prepare(value, a.Profile.Limits.MaxResultBytes)
				})
			}
			if err != nil {
				return failureResult(err), nil
			}
			return result, nil
		})
	}
	return s
}

func boundedResult(value any, limit int) (*sdk.CallToolResult, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	r := &sdk.CallToolResult{StructuredContent: json.RawMessage(b), Content: []sdk.Content{&sdk.TextContent{Text: string(b)}}}
	full, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(full) > min(limit, 1<<20) {
		return nil, database.Fail(contracts.ResourceLimit, "result exceeds payload budget; request less data", false)
	}
	return r, nil
}
func failureResult(err error) *sdk.CallToolResult {
	f := contracts.Failure{Code: contracts.ServiceUnavailable, Message: "service unavailable; retry after checking status", Retryable: true}
	var safe *database.Error
	switch {
	case errors.As(err, &safe):
		f = safe.Failure
	case errors.Is(err, context.Canceled):
		f = contracts.Failure{Code: contracts.Cancelled, Message: "operation cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		f = contracts.Failure{Code: contracts.QueryTimeout, Message: "operation deadline exceeded", Retryable: true}
	}
	r, _ := boundedResult(struct {
		Error contracts.Failure `json:"error"`
	}{f}, 1<<20)
	r.IsError = true
	return r
}
