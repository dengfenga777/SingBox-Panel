package agent

import (
	"context"
	"strings"
	"time"
)

// maxLogLines caps how much history the panel can pull in one request, so a
// noisy node can never flood the WebSocket.
const maxLogLines = 1000

// ReadLogs returns the most recent sing-box service logs from this node.
//
// The host init system selects either journalctl or the OpenRC-managed log
// file. Both commands are fixed and read-only, so the panel cannot run arbitrary
// commands through this path.
func ReadLogs(ctx context.Context, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	if lines > maxLogLines {
		lines = maxLogLines
	}
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	manager, err := currentServiceManager()
	if err != nil {
		return "", err
	}
	command, args := manager.LogCommand(lines, false)
	out, err := run(runCtx, command, args...)
	if err == nil && strings.TrimSpace(out) != "" &&
		!strings.Contains(out, "-- No entries --") {
		return out, nil
	}

	// If the native log source is unavailable, return the service status so the
	// operator at least sees whether sing-box is running and why it exited.
	statusOut := manager.Status(runCtx)
	if strings.TrimSpace(statusOut) != "" {
		return statusOut, nil
	}

	if strings.TrimSpace(out) != "" {
		return out, nil
	}
	return "(暂无日志，请确认 sing-box 服务已在节点上安装并运行)", nil
}
