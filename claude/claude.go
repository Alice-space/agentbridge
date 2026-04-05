// Package claude drives the claude CLI as a subprocess and parses its
// stream-json output into a plain text reply.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner executes the claude CLI for a single request.
type Runner struct {
	Command      string
	Timeout      time.Duration
	Env          map[string]string
	WorkspaceDir string
}

// Run is a convenience wrapper that runs without thread resumption or progress
// callbacks.
func (r Runner) Run(ctx context.Context, userText string) (string, error) {
	reply, _, err := r.RunWithThreadAndProgress(ctx, "", userText, "", nil, nil)
	return reply, err
}

// RunWithThreadAndProgress runs the claude CLI and returns the final assistant
// reply and the session ID to use for subsequent calls.
//
//   - threadID: resume an existing session when non-empty.
//   - userText: the fully assembled prompt (no further template rendering is done).
//   - model: overrides the CLI default when non-empty.
//   - env: merged over the process environment.
//   - onProgress: called with each intermediate assistant message; may be nil.
func (r Runner) RunWithThreadAndProgress(
	ctx context.Context,
	threadID string,
	userText string,
	model string,
	env map[string]string,
	onProgress func(step string),
) (string, string, error) {
	model = strings.TrimSpace(model)
	prompt := strings.TrimSpace(userText)
	if prompt == "" {
		return "", "", errors.New("empty prompt")
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 172800 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmdArgs := buildExecArgs(strings.TrimSpace(threadID), prompt, model)
	cmd := exec.CommandContext(tctx, r.Command, cmdArgs...)
	if strings.TrimSpace(r.WorkspaceDir) != "" {
		cmd.Dir = r.WorkspaceDir
	}
	cmd.Env = mergeEnv(mergeEnv(os.Environ(), r.Env), env)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", fmt.Errorf("create stdout pipe failed: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", "", fmt.Errorf("create stderr pipe failed: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("start claude process failed: %w", err)
	}

	var stderr bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderr, stderrPipe)
		close(stderrDone)
	}()

	var stdout bytes.Buffer
	activeThreadID := strings.TrimSpace(threadID)
	finalMessage := ""
	resultMessage := ""
	resultErrors := []string{}
	resultIsError := false

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 5*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		stdout.WriteString(line)
		stdout.WriteByte('\n')

		event := parseEventLine(line)
		if strings.TrimSpace(event.SessionID) != "" {
			activeThreadID = strings.TrimSpace(event.SessionID)
		}
		if strings.TrimSpace(event.AssistantText) != "" {
			finalMessage = strings.TrimSpace(event.AssistantText)
			if onProgress != nil {
				onProgress(finalMessage)
			}
		}
		if event.HasResultEvent {
			if strings.TrimSpace(event.ResultText) != "" {
				resultMessage = strings.TrimSpace(event.ResultText)
			}
			if len(event.ResultErrors) > 0 {
				resultErrors = event.ResultErrors
			}
			resultIsError = event.ResultIsError
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		<-stderrDone
		if errors.Is(tctx.Err(), context.DeadlineExceeded) {
			return "", activeThreadID, errors.New("claude timeout")
		}
		if errors.Is(tctx.Err(), context.Canceled) {
			return "", activeThreadID, context.Canceled
		}
		return "", activeThreadID, fmt.Errorf("read claude output failed: %w", scanErr)
	}

	err = cmd.Wait()
	<-stderrDone
	stderrText := strings.TrimSpace(stderr.String())
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return "", activeThreadID, errors.New("claude timeout")
	}
	if errors.Is(tctx.Err(), context.Canceled) {
		return "", activeThreadID, context.Canceled
	}
	if err != nil {
		detail := stderrText
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if len(detail) > 400 {
			detail = detail[:400]
		}
		return "", activeThreadID, fmt.Errorf("claude exec failed: %w (%s)", err, detail)
	}

	if resultIsError {
		detail := strings.TrimSpace(resultMessage)
		if detail == "" && len(resultErrors) > 0 {
			detail = strings.Join(resultErrors, "\n")
		}
		if detail == "" {
			detail = "unknown claude error"
		}
		return "", activeThreadID, fmt.Errorf("claude exec failed: %s", detail)
	}

	if strings.TrimSpace(finalMessage) == "" && strings.TrimSpace(resultMessage) != "" {
		finalMessage = strings.TrimSpace(resultMessage)
	}
	if finalMessage == "" {
		message, parseErr := ParseFinalMessage(stdout.String())
		if parseErr != nil {
			return "", activeThreadID, parseErr
		}
		finalMessage = strings.TrimSpace(message)
	}

	return finalMessage, activeThreadID, nil
}
