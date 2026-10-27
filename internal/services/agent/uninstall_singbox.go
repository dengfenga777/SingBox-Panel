package agent

import "context"

func UninstallSingbox(ctx context.Context) (string, error) {
	return runShell(ctx, uninstallSingboxScript)
}
