package opencode

import (
	"fmt"
	"strings"
)

func buildRunArgs(threadID, prompt, model string) []string {
	args := []string{"run"}
	if model = strings.TrimSpace(model); model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "--format", "json")
	if threadID = strings.TrimSpace(threadID); threadID != "" {
		args = append(args, "--session", threadID)
	}
	args = append(args, "--", prompt)
	return args
}

func buildLoginCheckArgs() []string {
	return []string{"run", "--help"}
}

func formatLoginError(runErr error, stderr string) error {
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "not authenticated") || strings.Contains(lower, "not logged in") || strings.Contains(lower, "no api key") {
		return fmt.Errorf("%w; opencode is not authenticated for this provider — run 'opencode auth login' first", runErr)
	}
	return runErr
}
