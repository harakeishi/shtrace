package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// newTestServer creates a Server with nil store/fts (sufficient for protocol-
// level tests that don't call storage).
func newTestServer() *Server {
	return &Server{}
}

func TestServe_Initialize(t *testing.T) {
	srv := newTestServer()
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}` + "\n"

	var buf bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &buf); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("decode response: %v (raw: %s)", err, buf.String())
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("wrong protocolVersion: %v", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "shtrace" {
		t.Errorf("wrong serverInfo.name: %v", info["name"])
	}
}

func TestServe_ToolsList(t *testing.T) {
	srv := newTestServer()
	input := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"

	var buf bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &buf); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 4 {
		t.Errorf("expected 4 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tt := range tools {
		m := tt.(map[string]any)
		names[m["name"].(string)] = true
	}
	for _, want := range []string{"get_session", "search_commands", "detect_test_runs", "compare_runs"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestServe_ParseError(t *testing.T) {
	srv := newTestServer()
	input := "not json\n"

	var buf bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &buf); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response for invalid JSON")
	}
	if resp.Error.Code != errCodeParse {
		t.Errorf("expected parse error code %d, got %d", errCodeParse, resp.Error.Code)
	}
	// JSON-RPC 2.0 §5: parse error response MUST contain "id": null.
	if string(resp.ID) != "null" {
		t.Errorf("expected id=null for parse error, got %s", resp.ID)
	}
}

func TestServe_MethodNotFound(t *testing.T) {
	srv := newTestServer()
	input := `{"jsonrpc":"2.0","id":3,"method":"unknown/method"}` + "\n"

	var buf bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &buf); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != errCodeNotFound {
		t.Errorf("expected not-found code %d, got %d", errCodeNotFound, resp.Error.Code)
	}
}

func TestServe_NotificationNoResponse(t *testing.T) {
	srv := newTestServer()
	// notifications/initialized is a JSON-RPC 2.0 Notification: the server
	// MUST NOT emit any response line per the spec.
	input := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n"

	var buf bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &buf); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for notification, got: %q", buf.String())
	}
}

func TestServe_MultipleRequests(t *testing.T) {
	srv := newTestServer()
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n"

	var buf bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &buf); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d: %q", len(lines), buf.String())
	}
	for i, l := range lines {
		var resp rpcResponse
		if err := json.Unmarshal([]byte(l), &resp); err != nil {
			t.Errorf("line %d: decode: %v", i, err)
		}
		if resp.Error != nil {
			t.Errorf("line %d: unexpected error: %+v", i, resp.Error)
		}
	}
}
