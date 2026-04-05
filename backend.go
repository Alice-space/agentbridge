// Package agentbridge provides a unified interface for running LLM agent CLIs
// (claude, codex, gemini, kimi, etc.) as subprocess backends.
//
// Instead of implementing LLM API clients, agentbridge drives each vendor's
// official CLI tool as a subprocess and exposes a single Backend interface.
// Prompts are assembled by the caller and passed as UserText; the library is
// responsible only for translating a RunRequest into the correct CLI invocation
// and parsing the CLI's stdout/stderr back into a RunResult.
package agentbridge

import "context"

// ProgressFunc is called with intermediate agent messages while a run is in
// progress. For codex it also receives file-change notifications prefixed with
// "[file_change] ".
type ProgressFunc func(step string)

// Usage holds token consumption reported by the CLI backend. Fields are zero
// when the backend does not expose usage information.
type Usage struct {
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
}

// TotalTokens returns InputTokens + OutputTokens.
func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens
}

// HasUsage reports whether any token counts were captured.
func (u Usage) HasUsage() bool {
	return u.InputTokens != 0 || u.CachedInputTokens != 0 || u.OutputTokens != 0
}

// ExecPolicyConfig controls sandbox and approval settings for backends that
// support them (currently codex only).
type ExecPolicyConfig struct {
	Sandbox        string
	AskForApproval string
	AddDirs        []string
}

// RunRequest is the input to Backend.Run. The caller is responsible for
// assembling the final prompt in UserText; the library does not perform any
// prompt templating.
type RunRequest struct {
	// ThreadID resumes an existing session when non-empty.
	ThreadID string
	// AgentName is informational metadata; not passed to the CLI.
	AgentName string
	// UserText is the fully assembled prompt sent to the CLI.
	UserText string
	// Scene is caller-defined metadata; not passed to the CLI.
	Scene string
	// Provider selects which backend to use when running through a MultiBackend.
	Provider string
	// Model overrides the default model for this request.
	Model string
	// Profile selects a named configuration profile defined in the backend config.
	Profile string
	// ReasoningEffort is forwarded to backends that support it (codex).
	ReasoningEffort string
	// Personality is forwarded to backends that accept it as a CLI flag (codex).
	Personality string
	// WorkspaceDir overrides the working directory for this request.
	WorkspaceDir string
	// ExecPolicy overrides sandbox/approval settings for this request (codex).
	ExecPolicy ExecPolicyConfig
	// Env is merged over the process environment before spawning the CLI.
	Env map[string]string
	// OnProgress receives streaming progress updates during execution.
	OnProgress ProgressFunc
}

// RunResult is the output from Backend.Run.
type RunResult struct {
	// Reply is the final assistant message produced by the CLI.
	Reply string
	// NextThreadID is the session/thread ID to pass as ThreadID on the next
	// call to continue the conversation.
	NextThreadID string
	// Usage contains token counts if the backend reported them.
	Usage Usage
}

// Backend runs a single LLM agent CLI.
type Backend interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// Provider wraps a Backend and is returned by NewProvider.
type Provider interface {
	Backend() Backend
}
