package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/saltyorg/sdhm/health"
)

func (d *Daemon) observeAliasConflicts(ctx context.Context, conflicts []AliasConflict) {
	if loopStreamStopReady(ctx, d.server.Done()) {
		return
	}
	if len(conflicts) == 0 {
		d.tracker.Recover(health.ConcernAliasConflicts)
		if d.lastAliasConflicts != "" {
			d.logger.Info("hosts alias conflicts resolved")
			d.lastAliasConflicts = ""
		}
		return
	}

	message := aliasConflictMessage(conflicts)
	if message != d.lastAliasConflicts {
		d.tracker.Fail(health.ConcernAliasConflicts, message)
		d.logger.Error("conflicting hostnames omitted", "conflicts", message)
		d.lastAliasConflicts = message
	}
}

func aliasConflictMessage(conflicts []AliasConflict) string {
	var message strings.Builder
	message.WriteString("omitted conflicting hostnames: ")
	for i, conflict := range conflicts {
		if i > 0 {
			message.WriteString("; ")
		}
		fmt.Fprintf(&message, "%q [", conflict.Hostname)
		for j, endpoint := range conflict.Endpoints {
			if j > 0 {
				message.WriteString(", ")
			}
			fmt.Fprintf(&message, "%s=%s", endpoint.Network, endpoint.IP)
		}
		message.WriteByte(']')
	}
	return message.String()
}
