//go:build !daemon

package main

import (
	"github.com/anivaryam/tunnel/internal/client"
	"github.com/spf13/cobra"
)

// daemonFlagSet is empty in the default build — daemon flags are not exposed.
type daemonFlagSet struct{}

func registerDaemonFlags(_ *cobra.Command, _ *daemonFlagSet) {}

func registerDaemonCommands(_ *cobra.Command, _ *daemonFlagSet) {}

func maybeDaemonize(_ daemonFlagSet, _ []string, _ *client.Client, _ string) error {
	return nil
}

func shouldExitParent(_ daemonFlagSet) bool { return false }

func attachIPC(_ *client.Client, _ daemonFlagSet) {}

func shutdownChildLogging() {}
