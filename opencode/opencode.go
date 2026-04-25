// Package opencode drives the opencode CLI as a subprocess and parses its
// JSON-lines output into a plain text reply, session ID, and token usage.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Runner executes the opencode CLI for a single request.
type Runner struct {
	Command      string
	Timeout      time.Duration
	Env          map[string]string
	WorkspaceDir string
}

type stepStart struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
}

type textEvent struct {
	Type string `json:"type"`
	Part struct {
		Text string `json:"text"`
	} `json:"part"`
}

type stepFinish struct {
	Type string `json:"type"`
	Part struct {
		Tokens struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			Reasoning  int64 `json:"reasoning"`
			Total      int64 `json:"total"`
			CacheWrite int64 `json:"cache_write,omitempty"`
			CacheRead  int64 `json:"cache_read,omitempty"`
			Cache      struct {
				Write int64 `json:"write"`
				Read  int64 `json:"read"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
}

// Run is a convenience wrapper that runs without thread resumption or progress
// callbacks.
func (r Runner) Run(ctx context.Context, userText string) (string, error) {
	reply, _, _, _, _, err := r.RunWithThreadAndProgress(ctx, "", userText, "", nil, nil)
	return reply, err
}

// RunWithThreadAndProgress runs the opencode CLI and returns the final reply,
// next session ID, and token usage.
//
//   - threadID: resume an existing session when non-empty.
//   - userText: the fully assembled prompt.
//   - model: the provider/model string (e.g. "deepseek/deepseek-v4-pro").
//   - env: merged over the process environment.
//   - onProgress: called with each streaming text chunk; may be nil.
func (r Runner) RunWithThreadAndProgress(
	ctx context.Context,
	threadID string,
	userText string,
	model string,
	env map[string]string,
	onProgress func(step string),
) (string, string, int64, int64, int64, error) {
	prompt := strings.TrimSpace(userText)
	if prompt == "" {
		return "", "", 0, 0, 0, errors.New("empty prompt")
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 172800 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmdArgs := buildRunArgs(threadID, prompt, model)
	cmd := exec.CommandContext(tctx, r.Command, cmdArgs...)
	if strings.TrimSpace(r.WorkspaceDir) != "" {
		cmd.Dir = r.WorkspaceDir
	}
	cmd.Env = mergeEnv(mergeEnv(os.Environ(), r.Env), env)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", 0, 0, 0, fmt.Errorf("create stdout pipe failed: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", "", 0, 0, 0, fmt.Errorf("create stderr pipe failed: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", "", 0, 0, 0, fmt.Errorf("start opencode process failed: %w", err)
	}

	var stderr bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderr, stderrPipe)
		close(stderrDone)
	}()

	var (
		reply         string
		nextThreadID  string
		inputTokens   int64
		outputTokens  int64
		cacheTokens   int64
		allText       []string
	)

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		parsed := parseOpenCodeLine(line, &inputTokens, &outputTokens, &cacheTokens, &nextThreadID, &allText)
		if parsed != "" && onProgress != nil {
			onProgress(parsed)
		}
	}

	err = cmd.Wait()
	<-stderrDone

	if len(allText) > 0 {
		reply = strings.TrimSpace(strings.Join(allText, "\n"))
	}

	cachedInputTokens := cacheTokens

	detail := strings.TrimSpace(stderr.String())
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return "", nextThreadID, inputTokens, outputTokens, cachedInputTokens, errors.New("opencode timeout")
	}
	if errors.Is(tctx.Err(), context.Canceled) {
		return "", nextThreadID, inputTokens, outputTokens, cachedInputTokens, context.Canceled
	}
	if err != nil {
		if detail == "" {
			detail = strings.TrimSpace(strings.Join(allText, " "))
		}
		if len(detail) > 400 {
			detail = detail[:400]
		}
		runErr := fmt.Errorf("opencode exec failed: %w (%s)", err, detail)
		return "", nextThreadID, inputTokens, outputTokens, cachedInputTokens, formatLoginError(runErr, detail)
	}
	if reply == "" {
		return "", nextThreadID, inputTokens, outputTokens, cachedInputTokens, errors.New("opencode returned no response text")
	}

	return reply, nextThreadID, inputTokens, outputTokens, cachedInputTokens, nil
}

func parseOpenCodeLine(line []byte, inputTokens, outputTokens, cacheTokens *int64, nextThreadID *string, allText *[]string) string {
	raw := json.RawMessage(line)
	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &peek); err != nil {
		return ""
	}

	switch peek.Type {
	case "step_start":
		var ev stepStart
		if err := json.Unmarshal(line, &ev); err != nil {
			return ""
		}
		if ev.SessionID != "" && *nextThreadID == "" {
			*nextThreadID = ev.SessionID
		}
		return ""
	case "text":
		var ev textEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return ""
		}
		text := strings.TrimSpace(ev.Part.Text)
		if text != "" {
			*allText = append(*allText, text)
			return text
		}
		return ""
	case "step_finish":
		var ev stepFinish
		if err := json.Unmarshal(line, &ev); err != nil {
			return ""
		}
		*inputTokens = ev.Part.Tokens.Input
		*outputTokens = ev.Part.Tokens.Output
		*cacheTokens = ev.Part.Tokens.CacheRead
		return ""
	}

	_ = raw
	return ""
}

func mergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	env := make([]string, len(base))
	copy(env, base)
	indexByKey := make(map[string]int, len(env))
	for i, item := range env {
		key := envKey(item)
		if key == "" {
			continue
		}
		indexByKey[key] = i
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := overrides[key]
		pair := key + "=" + value
		if idx, ok := indexByKey[key]; ok {
			env[idx] = pair
			continue
		}
		env = append(env, pair)
	}
	return env
}

func envKey(item string) string {
	idx := strings.Index(item, "=")
	if idx <= 0 {
		return ""
	}
	return item[:idx]
}
