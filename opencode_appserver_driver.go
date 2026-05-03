package agentbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

type openCodeAppServerDriver struct {
	cfg    OpenCodeConfig
	client *http.Client
	events chan TurnEvent

	cmd    *exec.Cmd
	stderr bytes.Buffer

	mu        sync.Mutex
	baseURL   string
	sessionID string
	activeID  string
	closed    bool
	nextID    atomic.Uint64
	closeOnce sync.Once
}

func newOpenCodeAppServerDriver(cfg OpenCodeConfig) *openCodeAppServerDriver {
	return &openCodeAppServerDriver{
		cfg:    cfg,
		client: &http.Client{},
		events: make(chan TurnEvent, 128),
	}
}

func (d *openCodeAppServerDriver) SteerMode() SteerMode {
	return SteerModeNativeEnqueue
}

func (d *openCodeAppServerDriver) StartTurn(ctx context.Context, req RunRequest) (TurnRef, error) {
	if err := d.ensureServer(ctx, req); err != nil {
		return TurnRef{}, err
	}
	sessionID, err := d.ensureSession(ctx, req)
	if err != nil {
		return TurnRef{}, err
	}
	turnID := "opencode-" + fmt.Sprint(d.nextID.Add(1))
	d.mu.Lock()
	d.activeID = turnID
	d.mu.Unlock()

	turn := TurnRef{ThreadID: sessionID, TurnID: turnID}
	d.emit(TurnEvent{Provider: ProviderOpenCode, ThreadID: sessionID, TurnID: turnID, Kind: TurnEventStarted})
	go d.runPrompt(ctx, turn, req)
	return turn, nil
}

func (d *openCodeAppServerDriver) SteerTurn(ctx context.Context, turn TurnRef, req RunRequest) error {
	sessionID := firstNonEmpty(turn.ThreadID, d.currentSessionID())
	if sessionID == "" {
		return errors.New("opencode app-server has no active session")
	}
	if err := d.postNoContent(ctx, "/session/"+url.PathEscape(sessionID)+"/prompt_async", d.promptBody(req), req); err != nil {
		return err
	}
	d.emit(TurnEvent{
		Provider: ProviderOpenCode,
		ThreadID: sessionID,
		TurnID:   strings.TrimSpace(turn.TurnID),
		Kind:     TurnEventSteerConsumed,
		Text:     strings.TrimSpace(req.UserText),
	})
	return nil
}

func (d *openCodeAppServerDriver) InterruptTurn(ctx context.Context, turn TurnRef) error {
	sessionID := firstNonEmpty(turn.ThreadID, d.currentSessionID())
	if sessionID == "" {
		return nil
	}
	return d.postNoContent(ctx, "/session/"+url.PathEscape(sessionID)+"/abort", nil, RunRequest{})
}

func (d *openCodeAppServerDriver) Events() <-chan TurnEvent {
	return d.events
}

func (d *openCodeAppServerDriver) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		cmd := d.cmd
		d.cmd = nil
		d.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			err = cmd.Wait()
		}
		close(d.events)
	})
	return err
}

func (d *openCodeAppServerDriver) ensureServer(ctx context.Context, req RunRequest) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return ErrInteractiveClosed
	}
	if d.baseURL != "" {
		d.mu.Unlock()
		return nil
	}
	if serverURL := strings.TrimSpace(d.cfg.ServerURL); serverURL != "" {
		d.baseURL = strings.TrimRight(serverURL, "/")
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()

	command := strings.TrimSpace(d.cfg.Command)
	if command == "" {
		command = "opencode"
	}
	cmd := exec.Command(command, "serve", "--hostname", "127.0.0.1", "--port", "0")
	if cwd := firstNonEmpty(req.WorkspaceDir, d.cfg.WorkspaceDir); cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = mergeProcessEnv(mergeProcessEnv(os.Environ(), d.cfg.Env), req.Env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create opencode stdout pipe failed: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("create opencode stderr pipe failed: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start opencode serve failed: %w", err)
	}

	urlCh := make(chan string, 1)
	go d.scanOpenCodeServeOutput(stdout, urlCh)
	go func() {
		_, _ = io.Copy(&d.stderr, stderr)
	}()

	select {
	case serverURL := <-urlCh:
		if serverURL == "" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("opencode serve exited before reporting URL: %s", strings.TrimSpace(d.stderr.String()))
		}
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return ErrInteractiveClosed
		}
		d.cmd = cmd
		d.baseURL = strings.TrimRight(serverURL, "/")
		d.mu.Unlock()
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ctx.Err()
	}
}

func (d *openCodeAppServerDriver) scanOpenCodeServeOutput(stdout io.Reader, urlCh chan<- string) {
	defer close(urlCh)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if serverURL := extractHTTPURL(line); serverURL != "" {
			urlCh <- serverURL
			return
		}
	}
	urlCh <- ""
}

func (d *openCodeAppServerDriver) ensureSession(ctx context.Context, req RunRequest) (string, error) {
	d.mu.Lock()
	if d.sessionID != "" {
		sessionID := d.sessionID
		d.mu.Unlock()
		return sessionID, nil
	}
	d.mu.Unlock()

	sessionID := strings.TrimSpace(req.ThreadID)
	if sessionID == "" {
		var created struct {
			ID string `json:"id"`
		}
		if err := d.postJSON(ctx, "/session", map[string]any{}, req, &created); err != nil {
			return "", err
		}
		sessionID = strings.TrimSpace(created.ID)
	}
	if sessionID == "" {
		return "", errors.New("opencode app-server returned no session id")
	}
	d.mu.Lock()
	if d.sessionID == "" {
		d.sessionID = sessionID
	}
	sessionID = d.sessionID
	d.mu.Unlock()
	return sessionID, nil
}

func (d *openCodeAppServerDriver) runPrompt(ctx context.Context, turn TurnRef, req RunRequest) {
	var response openCodePromptResponse
	err := d.postJSON(ctx, "/session/"+url.PathEscape(turn.ThreadID)+"/message", d.promptBody(req), req, &response)
	if err != nil {
		kind := TurnEventError
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind = TurnEventInterrupted
		}
		d.emit(TurnEvent{Provider: ProviderOpenCode, ThreadID: turn.ThreadID, TurnID: turn.TurnID, Kind: kind, Err: err})
		return
	}
	if text := strings.TrimSpace(response.Text()); text != "" {
		d.emit(TurnEvent{Provider: ProviderOpenCode, ThreadID: turn.ThreadID, TurnID: turn.TurnID, Kind: TurnEventAssistantText, Text: text})
	}
	if response.Info.Error != nil {
		d.emit(TurnEvent{
			Provider: ProviderOpenCode,
			ThreadID: turn.ThreadID,
			TurnID:   turn.TurnID,
			Kind:     TurnEventError,
			Err:      fmt.Errorf("opencode turn failed: %s", openCodeErrorMessage(response.Info.Error)),
			Usage:    response.Info.Usage(),
		})
		return
	}
	d.emit(TurnEvent{
		Provider: ProviderOpenCode,
		ThreadID: turn.ThreadID,
		TurnID:   turn.TurnID,
		Kind:     TurnEventCompleted,
		Usage:    response.Info.Usage(),
	})
}

func (d *openCodeAppServerDriver) promptBody(req RunRequest) map[string]any {
	body := map[string]any{
		"parts": []map[string]any{{
			"type": "text",
			"text": strings.TrimSpace(req.UserText),
		}},
	}
	if model := firstNonEmpty(req.Model, d.cfg.Model); model != "" {
		providerID, modelID := splitOpenCodeModel(model)
		if providerID != "" && modelID != "" {
			body["model"] = map[string]any{"providerID": providerID, "modelID": modelID}
		}
	}
	if variant := firstNonEmpty(req.Variant, d.cfg.Variant); variant != "" {
		body["variant"] = variant
	}
	if agent := strings.TrimSpace(req.Profile); agent != "" {
		resolvedAgent := agent
		if override, ok := d.cfg.ProfileOverrides[agent]; ok && strings.TrimSpace(override.ProviderProfile) != "" {
			resolvedAgent = strings.TrimSpace(override.ProviderProfile)
		}
		if isKnownOpenCodeAgent(resolvedAgent) {
			body["agent"] = resolvedAgent
		}
	}
	return body
}

func (d *openCodeAppServerDriver) postNoContent(ctx context.Context, path string, body any, req RunRequest) error {
	return d.postJSON(ctx, path, body, req, nil)
}

func (d *openCodeAppServerDriver) postJSON(ctx context.Context, path string, body any, req RunRequest, out any) error {
	endpoint, err := d.endpoint(path, req)
	if err != nil {
		return err
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, payload)
	if err != nil {
		return err
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode app-server %s failed status=%d body=%s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			d.resetServerForNextRequest()
		}
		msg := fmt.Sprintf("decode opencode app-server %s response failed: %v", path, err)
		if stderr := strings.TrimSpace(d.stderr.String()); stderr != "" {
			msg += " (server stderr: " + stderr + ")"
		}
		return errors.New(msg)
	}
	return nil
}

func (d *openCodeAppServerDriver) endpoint(path string, req RunRequest) (string, error) {
	d.mu.Lock()
	base := d.baseURL
	d.mu.Unlock()
	if base == "" {
		return "", errors.New("opencode app-server is not started")
	}
	u, err := url.Parse(base + path)
	if err != nil {
		return "", err
	}
	if directory := firstNonEmpty(req.WorkspaceDir, d.cfg.WorkspaceDir); directory != "" {
		q := u.Query()
		q.Set("directory", directory)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func (d *openCodeAppServerDriver) currentSessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessionID
}

func (d *openCodeAppServerDriver) resetServerForNextRequest() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.baseURL = ""
	d.sessionID = ""
	d.activeID = ""
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
		_ = d.cmd.Wait()
	}
	d.cmd = nil
}

func (d *openCodeAppServerDriver) emit(event TurnEvent) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return
	}
	select {
	case d.events <- event:
	default:
	}
}

type openCodePromptResponse struct {
	Info  openCodeAssistantInfo `json:"info"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

func (r openCodePromptResponse) Text() string {
	parts := make([]string, 0, len(r.Parts))
	for _, part := range r.Parts {
		if part.Type != "text" {
			continue
		}
		if text := strings.TrimSpace(part.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

type openCodeAssistantInfo struct {
	Error  map[string]any `json:"error"`
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

func (i openCodeAssistantInfo) Usage() Usage {
	return Usage{
		InputTokens:       i.Tokens.Input,
		CachedInputTokens: i.Tokens.Cache.Read,
		OutputTokens:      i.Tokens.Output,
	}
}

func (i openCodeAssistantInfo) Message() string {
	return openCodeErrorMessage(i.Error)
}

func openCodeErrorMessage(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	if data, _ := payload["data"].(map[string]any); len(data) > 0 {
		if msg := stringFromMap(data, "message"); msg != "" {
			return msg
		}
	}
	if msg := stringFromMap(payload, "message"); msg != "" {
		return msg
	}
	if name := stringFromMap(payload, "name"); name != "" {
		return name
	}
	return "unknown error"
}

func splitOpenCodeModel(model string) (string, string) {
	model = strings.TrimSpace(model)
	providerID, modelID, ok := strings.Cut(model, "/")
	if !ok {
		return "", ""
	}
	return strings.TrimSpace(providerID), strings.TrimSpace(modelID)
}

func extractHTTPURL(line string) string {
	for _, field := range strings.Fields(line) {
		field = strings.TrimRight(field, ".,)")
		if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
			return field
		}
	}
	return ""
}

var knownOpenCodeAgents = map[string]bool{
	"build":    true,
	"explore":  true,
	"general":  true,
	"plan":     true,
}

func isKnownOpenCodeAgent(agent string) bool {
	return knownOpenCodeAgents[strings.ToLower(strings.TrimSpace(agent))]
}
