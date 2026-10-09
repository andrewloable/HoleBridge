package cli

import "context"

// serviceRequest is a control request from the Service Control Manager, in the portable form the handler core takes.
// The svc package is Windows-only, so the core does not use its svc.Cmd values; winsvc_windows.go maps them.
type serviceRequest int

const (
	requestInterrogate serviceRequest = iota + 1 // the Service Control Manager asks for the current state
	requestStop                                  // the Service Control Manager asks the service to stop
	requestShutdown                              // the system is shutting down
)

// serviceState is the portable form of a state the service reports to the Service Control Manager.
type serviceState int

const (
	stateStartPending serviceState = iota + 1
	stateRunning
	stateStopPending
)

// serveHost is the portable core of the Windows service handler. It reports stateStartPending, runs the host through
// run (in its own goroutine), and reports stateRunning. An Interrogate request reports the current state again and
// leaves the host running. A Stop or Shutdown request reports stateStopPending and cancels the context that run was
// given (which stops the host as an interrupt does). serveHost returns once run has returned, whether or not a request
// was made: a host that ends on its own ends the service. The result is (false, 0) when run returned nil, and
// (true, code) with a non-zero service specific code when run returned an error, whether before or after a request,
// so the Service Control Manager sees a failure and not a clean exit. Each state is passed to report.
func serveHost(run func(ctx context.Context) error, requests <-chan serviceRequest, report func(serviceState)) (svcSpecificEC bool, exitCode uint32) {
	panic("not implemented")
}

// serviceCommandLine returns the command line the service is registered with: the executable, then --config, the
// config directory and host. The executable and the directory are quoted where the Windows command line rules need it.
// The directory is passed explicitly because the Service Control Manager starts the service under its own account,
// whose default config directory is not the one the installing user has.
func serviceCommandLine(exe, configDir string) string {
	panic("not implemented")
}
