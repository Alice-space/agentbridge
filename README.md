# agentbridge

A unified Go library for driving LLM agent CLIs as subprocess backends.

Instead of implementing LLM API clients, agentbridge drives each vendor's official CLI tool as a subprocess and exposes a single `Backend` interface. The caller assembles the prompt; agentbridge translates a `RunRequest` into the correct CLI invocation and parses the output back into a `RunResult`.

## Supported Backends

| Provider | CLI | Thread resume | Token usage |
|----------|-----|:---:|:---:|
| `codex`  | [codex](https://github.com/openai/codex) | ✓ | ✓ |
| `claude` | [claude](https://github.com/anthropics/claude-code) | ✓ | ✓ |
| `gemini` | [gemini](https://github.com/google-gemini/gemini-cli) | ✓ | ✓ |
| `kimi`   | kimi CLI | ✓ | — |

## Installation

```bash
go get github.com/Alice-space/agentbridge
```

**Zero external dependencies.** Only the Go standard library is required.

## Quick Start

```go
package main

import (
    "context"
    "fmt"

    agentbridge "github.com/Alice-space/agentbridge"
)

func main() {
    backend, err := agentbridge.NewBackend(agentbridge.FactoryConfig{
        Provider: agentbridge.ProviderClaude,
        Claude: agentbridge.ClaudeConfig{
            Command: "claude",
        },
    })
    if err != nil {
        panic(err)
    }

    result, err := backend.Run(context.Background(), agentbridge.RunRequest{
        UserText: "What is 2 + 2?",
    })
    if err != nil {
        panic(err)
    }
    fmt.Println(result.Reply)
}
```

## Multi-backend Routing

Route requests to different CLI backends based on `RunRequest.Provider`:

```go
multi, err := agentbridge.NewMultiBackend("codex", map[string]agentbridge.Backend{
    "codex":  codexBackend,
    "claude": claudeBackend,
    "gemini": geminiBackend,
})

result, err := multi.Run(ctx, agentbridge.RunRequest{
    Provider: "claude",
    UserText:  "Hello!",
})
```

## Thread Resumption

All backends support resuming a previous conversation via `ThreadID`:

```go
// First turn
result, err := backend.Run(ctx, agentbridge.RunRequest{
    UserText: "Start a new task",
})

// Second turn — resume the same session
result2, err := backend.Run(ctx, agentbridge.RunRequest{
    ThreadID: result.NextThreadID,
    UserText: "Continue from where you left off",
})
```

## Streaming Progress

Receive intermediate messages during a long-running codex session:

```go
result, err := backend.Run(ctx, agentbridge.RunRequest{
    UserText: "Refactor this file",
    OnProgress: func(step string) {
        if strings.HasPrefix(step, "[file_change] ") {
            fmt.Println("File changed:", strings.TrimPrefix(step, "[file_change] "))
        } else {
            fmt.Println("Agent:", step)
        }
    },
})
```

## Design

**The library does not assemble prompts.** `RunRequest.UserText` is passed directly to the CLI. The caller is responsible for constructing the final prompt (system instructions, reply tokens, etc.) before calling `Run`.

**No logging.** The library returns errors and lets callers decide how to log them.

**Provider-specific flags** (model, sandbox policy, reasoning effort, personality) are mapped to the appropriate CLI arguments by each backend.

## Configuration Reference

### CodexConfig

```go
agentbridge.CodexConfig{
    Command:            "codex",          // CLI binary name or path
    Timeout:            10 * time.Minute, // overall execution timeout
    DefaultIdleTimeout: 15 * time.Minute, // idle timeout (default reasoning)
    HighIdleTimeout:    30 * time.Minute, // idle timeout for high reasoning
    XHighIdleTimeout:   60 * time.Minute, // idle timeout for xhigh reasoning
    Model:              "o4-mini",
    ReasoningEffort:    "medium",
    Env:                map[string]string{"MY_KEY": "value"},
    WorkspaceDir:       "/path/to/project",
    DefaultExecPolicy: agentbridge.ExecPolicyConfig{
        Sandbox:        "workspace-write",
        AskForApproval: "never",
    },
    ProfileOverrides: map[string]agentbridge.ProfileRunnerConfig{
        "executor": {ReasoningEffort: "xhigh"},
    },
}
```

### ClaudeConfig / GeminiConfig / KimiConfig

```go
agentbridge.ClaudeConfig{
    Command:      "claude",
    Timeout:      10 * time.Minute,
    Env:          map[string]string{},
    WorkspaceDir: "/path/to/project",
    ProfileOverrides: map[string]agentbridge.ProfileRunnerConfig{
        "fast": {Command: "claude-fast"},
    },
}
```

## License

MIT
