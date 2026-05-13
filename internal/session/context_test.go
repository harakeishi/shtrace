package session

import (
	"testing"
)

func TestFromEnv_NewSession_WhenNoSessionIDPresent(t *testing.T) {
	env := map[string]string{}

	ctx, err := FromEnv(env, fixedIDGen("01900000-0000-7000-8000-000000000001", "01900000-0000-7000-8000-0000000000aa"))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}

	if ctx.SessionID != "01900000-0000-7000-8000-000000000001" {
		t.Fatalf("SessionID = %q, want new generated id", ctx.SessionID)
	}
	if ctx.SpanID != "01900000-0000-7000-8000-0000000000aa" {
		t.Fatalf("SpanID = %q, want new generated id", ctx.SpanID)
	}
	if ctx.ParentSpanID != "" {
		t.Fatalf("ParentSpanID = %q, want empty for root span", ctx.ParentSpanID)
	}
	if !ctx.IsRoot {
		t.Fatalf("IsRoot = false, want true when no SHTRACE_SESSION_ID is set")
	}
}

func TestFromEnv_ChildSession_InheritsSessionID(t *testing.T) {
	env := map[string]string{
		"SHTRACE_SESSION_ID":     "01900000-0000-7000-8000-000000000001",
		"SHTRACE_PARENT_SPAN_ID": "01900000-0000-7000-8000-0000000000aa",
	}

	ctx, err := FromEnv(env, fixedIDGen("ignored-session", "01900000-0000-7000-8000-0000000000bb"))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}

	if ctx.SessionID != "01900000-0000-7000-8000-000000000001" {
		t.Fatalf("SessionID = %q, want inherited value", ctx.SessionID)
	}
	if ctx.ParentSpanID != "01900000-0000-7000-8000-0000000000aa" {
		t.Fatalf("ParentSpanID = %q, want value from env", ctx.ParentSpanID)
	}
	if ctx.SpanID != "01900000-0000-7000-8000-0000000000bb" {
		t.Fatalf("SpanID = %q, want freshly generated id", ctx.SpanID)
	}
	if ctx.IsRoot {
		t.Fatalf("IsRoot = true, want false for child span")
	}
}

func TestFromEnv_ParsesTagsJSON(t *testing.T) {
	env := map[string]string{
		"SHTRACE_TAGS": `{"pr_number":"42","agent_name":"claude"}`,
	}

	ctx, err := FromEnv(env, fixedIDGen("s", "p"))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}

	if got := ctx.Tags["pr_number"]; got != "42" {
		t.Fatalf("Tags[pr_number] = %q, want 42", got)
	}
	if got := ctx.Tags["agent_name"]; got != "claude" {
		t.Fatalf("Tags[agent_name] = %q, want claude", got)
	}
}

func TestFromEnv_InvalidTagsJSON_ReturnsError(t *testing.T) {
	env := map[string]string{
		"SHTRACE_TAGS": "{not-json",
	}

	_, err := FromEnv(env, fixedIDGen("s", "p"))
	if err == nil {
		t.Fatalf("FromEnv: expected error for invalid JSON tags, got nil")
	}
}

func TestContext_ChildEnv_PropagatesIDsAndTags(t *testing.T) {
	ctx := &Context{
		SessionID: "sess-1",
		SpanID:    "span-1",
		Tags:      map[string]string{"pr_number": "42"},
	}

	got := ctx.ChildEnv()

	if got["SHTRACE_SESSION_ID"] != "sess-1" {
		t.Fatalf("ChildEnv SHTRACE_SESSION_ID = %q, want sess-1", got["SHTRACE_SESSION_ID"])
	}
	if got["SHTRACE_PARENT_SPAN_ID"] != "span-1" {
		t.Fatalf("ChildEnv SHTRACE_PARENT_SPAN_ID = %q, want span-1", got["SHTRACE_PARENT_SPAN_ID"])
	}
	if got["SHTRACE_TAGS"] == "" {
		t.Fatalf("ChildEnv SHTRACE_TAGS missing")
	}
}

// fixedIDGen returns an IDGenerator that yields the given session and span IDs
// in order. Tests only need predictable IDs.
func fixedIDGen(sessionID, spanID string) IDGenerator {
	return IDGenerator{
		NewSessionID: func() (string, error) { return sessionID, nil },
		NewSpanID:    func() (string, error) { return spanID, nil },
	}
}
