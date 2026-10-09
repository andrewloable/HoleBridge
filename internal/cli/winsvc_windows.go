//go:build windows

package cli

import (
	"context"
	"errors"

	"golang.org/x/sys/windows/svc"
)

// serviceHandler is the svc.Handler that runs holebridge host when the Service Control Manager starts it. It is a thin
// adapter over serveHost (winsvc.go): run is the host, which serveHost runs until a Stop or Shutdown request.
type serviceHandler struct {
	run func(ctx context.Context) error
}

// Execute implements svc.Handler by calling serveHost. It forwards each change request from r as a serviceRequest
// (see serviceCmdOf), reports each state as an svc.Status on changes (see svcStatusOf), and returns serveHost's exit
// code.
func (h serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	panic("not implemented")
}

// serviceCmdOf maps a change request from the Service Control Manager to the portable request that serveHost takes.
// ok is false for requests the host does not act on, such as pause.
func serviceCmdOf(c svc.Cmd) (req serviceRequest, ok bool) {
	panic("not implemented")
}

// svcStatusOf maps a state that serveHost reports to the svc.Status the Service Control Manager is told. A running
// service accepts stop and shutdown requests.
func svcStatusOf(s serviceState) svc.Status {
	panic("not implemented")
}

// serviceInstall is holebridge service-install. It registers this holebridge with the Service Control Manager under a
// fixed service name, to run holebridge host with the config directory of this command at boot. It has the Command
// signature so that the IMPL task can register it in the command table.
func serviceInstall(args []string, env Env, configDir string) error {
	return errors.ErrUnsupported
}

// serviceUninstall is holebridge service-uninstall. It removes the service that serviceInstall registered.
func serviceUninstall(args []string, env Env, configDir string) error {
	return errors.ErrUnsupported
}
