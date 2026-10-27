package agent

import (
	"context"
	"fmt"
)

const systemdUnitPath = "/org/freedesktop/systemd1/unit/sing_2dbox_2eservice"

// allowedServiceActions is the common command whitelist for systemd and OpenRC.
var allowedServiceActions = map[string]bool{
	"start": true, "stop": true, "restart": true,
	"reload": true, "enable": true, "disable": true,
}

// ServiceAction runs a whitelisted service action through the host init system.
func ServiceAction(ctx context.Context, action string) (string, error) {
	manager, err := currentServiceManager()
	if err != nil {
		return "", err
	}
	if action == "status" {
		return manager.Status(ctx), nil
	}
	if !allowedServiceActions[action] {
		return "", fmt.Errorf("disallowed service action %q", action)
	}
	return manager.Action(ctx, action)
}

// ServiceActive reports whether sing-box is active under the host init system.
func ServiceActive(ctx context.Context) bool {
	manager, err := currentServiceManager()
	return err == nil && manager.Active(ctx)
}

// ServiceEnabled reports whether sing-box is enabled at boot.
func ServiceEnabled(ctx context.Context) bool {
	manager, err := currentServiceManager()
	return err == nil && manager.Enabled(ctx)
}
