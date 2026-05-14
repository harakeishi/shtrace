// Package mcp implements a Model Context Protocol server over stdio using
// JSON-RPC 2.0. It exposes four tools that let AI agents query shtrace's
// recorded execution history without external SDK dependencies.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/harakeishi/shtrace/internal/storage"
)

// rpcRequest is a JSON-RPC 2.0 request envelope.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response envelope.
// skip is an internal flag: when true, Serve does not write this response to
// the wire. Used for JSON-RPC 2.0 Notifications, which must not receive a reply.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	skip    bool
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	errCodeParse    = -32700
	errCodeInvalid  = -32600
	errCodeNotFound = -32601
	errCodeParams   = -32602
	errCodeInternal = -32603
)

// Server holds the dependencies shared across all tool handlers.
type Server struct {
	store   *storage.Store
	fts     *storage.FTSStore
	dataDir string
}

// NewServer constructs a Server. fts may be nil when the FTS index is absent.
func NewServer(store *storage.Store, fts *storage.FTSStore, dataDir string) *Server {
	return &Server{store: store, fts: fts, dataDir: dataDir}
}

// Serve reads newline-delimited JSON-RPC requests from r and writes responses
// to w until r returns EOF or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	enc := json.NewEncoder(w)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 4<<20) // up to 4 MB per message

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// JSON-RPC 2.0 §5: when the request cannot be parsed, id MUST be null.
			if encErr := enc.Encode(rpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: errCodeParse, Message: "parse error: " + err.Error()},
			}); encErr != nil {
				return fmt.Errorf("mcp: encode error response: %w", encErr)
			}
			continue
		}
		if req.JSONRPC != "2.0" {
			if encErr := enc.Encode(rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: errCodeInvalid, Message: "invalid request: jsonrpc must be \"2.0\""},
			}); encErr != nil {
				return fmt.Errorf("mcp: encode error response: %w", encErr)
			}
			continue
		}

		resp := s.dispatch(ctx, &req)
		// JSON-RPC 2.0 §4: a Notification is a request with no "id" field.
		// The server MUST NOT send a response for any Notification, regardless
		// of whether the method was recognised. We check both the explicit
		// skip flag (set by known notification methods) and req.ID being absent.
		if resp.skip || len(req.ID) == 0 {
			continue
		}
		resp.JSONRPC = "2.0"
		resp.ID = req.ID
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("mcp: encode response: %w", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcp: scan: %w", err)
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, req *rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize()
	case "notifications/initialized":
		// JSON-RPC 2.0 Notification: no response must be sent.
		return rpcResponse{skip: true}
	case "tools/list":
		return s.handleToolsList()
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	case "ping":
		return rpcResponse{Result: map[string]any{}}
	default:
		return rpcResponse{Error: &rpcError{Code: errCodeNotFound, Message: "method not found: " + req.Method}}
	}
}

func (s *Server) handleInitialize() rpcResponse {
	return rpcResponse{
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "shtrace",
				"version": "0.1.0",
			},
		},
	}
}

func (s *Server) handleToolsList() rpcResponse {
	return rpcResponse{Result: map[string]any{"tools": toolDefinitions()}}
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolsCall(ctx context.Context, req *rpcRequest) rpcResponse {
	var p toolCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return rpcResponse{Error: &rpcError{Code: errCodeParams, Message: "invalid params: " + err.Error()}}
	}

	var (
		result any
		err    error
	)
	switch p.Name {
	case "get_session":
		result, err = s.toolGetSession(ctx, p.Arguments)
	case "search_commands":
		result, err = s.toolSearchCommands(ctx, p.Arguments)
	case "detect_test_runs":
		result, err = s.toolDetectTestRuns(ctx, p.Arguments)
	case "compare_runs":
		result, err = s.toolCompareRuns(ctx, p.Arguments)
	default:
		return rpcResponse{Error: &rpcError{Code: errCodeNotFound, Message: "tool not found: " + p.Name}}
	}

	if err != nil {
		return rpcResponse{
			Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			},
		}
	}

	text, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		return rpcResponse{
			Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": "internal error: marshal result: " + marshalErr.Error()}},
				"isError": true,
			},
		}
	}
	return rpcResponse{
		Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(text)}},
		},
	}
}

// toolDefinitions returns the MCP tool schema list for all four tools.
func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "get_session",
			"description": "Retrieve full session metadata and all spans (recorded command invocations) for a given session ID.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"session_id"},
				"properties": map[string]any{
					"session_id": map[string]any{
						"type":        "string",
						"description": "The shtrace session ID to retrieve.",
					},
				},
			},
		},
		{
			"name":        "search_commands",
			"description": "Full-text search over recorded command outputs. Returns matching span IDs and text snippets.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"query"},
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "SQLite FTS5 query string (e.g. \"error AND build\").",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of results to return (default 20, max 200).",
					},
				},
			},
		},
		{
			"name":        "detect_test_runs",
			"description": "Detect test framework executions (pytest, jest, vitest, phpunit, go test, rspec) within a session and return results with pass/fail counts.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"session_id"},
				"properties": map[string]any{
					"session_id": map[string]any{
						"type":        "string",
						"description": "The shtrace session ID to inspect for test runs.",
					},
				},
			},
		},
		{
			"name":        "compare_runs",
			"description": "Compare detected test results between two sessions. Returns tests that changed status (newly failing, newly passing, etc.).",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"session_a", "session_b"},
				"properties": map[string]any{
					"session_a": map[string]any{
						"type":        "string",
						"description": "Session ID for the baseline run.",
					},
					"session_b": map[string]any{
						"type":        "string",
						"description": "Session ID for the comparison run.",
					},
				},
			},
		},
	}
}
