// Package session models the session/span context that every shtrace
// invocation lives inside, plus the propagation rules described in the plan
// (SHTRACE_SESSION_ID / SHTRACE_PARENT_SPAN_ID / SHTRACE_TAGS).
package session

import (
	"encoding/json"
	"fmt"
)

const (
	EnvSessionID    = "SHTRACE_SESSION_ID"
	EnvParentSpanID = "SHTRACE_PARENT_SPAN_ID"
	EnvTags         = "SHTRACE_TAGS"
)

// Context is the resolved session/span identity for one shtrace invocation.
type Context struct {
	SessionID    string
	SpanID       string
	ParentSpanID string
	IsRoot       bool
	Tags         map[string]string
}

// IDGenerator produces session and span IDs. Injected for testability; the
// production generator yields UUIDv7 values.
type IDGenerator struct {
	NewSessionID func() (string, error)
	NewSpanID    func() (string, error)
}

// FromEnv resolves a Context from a flattened environment map. It treats an
// absent SHTRACE_SESSION_ID as "start a new root session"; otherwise it joins
// the existing session as a child span.
func FromEnv(env map[string]string, gen IDGenerator) (*Context, error) {
	ctx := &Context{
		Tags: map[string]string{},
	}

	if raw, ok := env[EnvTags]; ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &ctx.Tags); err != nil {
			return nil, fmt.Errorf("parse %s: %w", EnvTags, err)
		}
	}

	if sid, ok := env[EnvSessionID]; ok && sid != "" {
		ctx.SessionID = sid
		ctx.ParentSpanID = env[EnvParentSpanID]
		ctx.IsRoot = false
	} else {
		sid, err := gen.NewSessionID()
		if err != nil {
			return nil, fmt.Errorf("generate session id: %w", err)
		}
		ctx.SessionID = sid
		ctx.IsRoot = true
	}

	spanID, err := gen.NewSpanID()
	if err != nil {
		return nil, fmt.Errorf("generate span id: %w", err)
	}
	ctx.SpanID = spanID

	return ctx, nil
}

// ChildEnv returns the env vars that must be exported to a child process so
// that nested shtrace invocations attach to the same session.
func (c *Context) ChildEnv() map[string]string {
	out := map[string]string{
		EnvSessionID:    c.SessionID,
		EnvParentSpanID: c.SpanID,
	}
	if len(c.Tags) > 0 {
		if b, err := json.Marshal(c.Tags); err == nil {
			out[EnvTags] = string(b)
		}
	}
	return out
}
