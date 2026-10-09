//go:build windows

package cli

import (
	"testing"

	"golang.org/x/sys/windows/svc"
)

// TestServiceAdapterMapsRequestsAndStates checks the two mappings the svc adapter makes: the requests the Service
// Control Manager sends, to the portable requests serveHost takes, and the portable states, to the svc.Status values
// the Service Control Manager is told. Pause is not a request the host acts on, so it does not map.
func TestServiceAdapterMapsRequestsAndStates(t *testing.T) {
	defer failOnPanic(t)

	cmds := []struct {
		in   svc.Cmd
		want serviceRequest
	}{
		{svc.Interrogate, requestInterrogate},
		{svc.Stop, requestStop},
		{svc.Shutdown, requestShutdown},
	}
	for _, c := range cmds {
		got, ok := serviceCmdOf(c.in)
		if !ok || got != c.want {
			t.Errorf("serviceCmdOf(%v) = %v, %v; want %v, true", c.in, got, ok, c.want)
		}
	}
	if _, ok := serviceCmdOf(svc.Pause); ok {
		t.Error("serviceCmdOf(svc.Pause) reports ok; want false")
	}

	if st := svcStatusOf(stateStartPending); st.State != svc.StartPending {
		t.Errorf("svcStatusOf(stateStartPending).State = %v; want svc.StartPending", st.State)
	}
	running := svcStatusOf(stateRunning)
	if running.State != svc.Running {
		t.Errorf("svcStatusOf(stateRunning).State = %v; want svc.Running", running.State)
	}
	if running.Accepts&svc.AcceptStop == 0 || running.Accepts&svc.AcceptShutdown == 0 {
		t.Errorf("svcStatusOf(stateRunning).Accepts = %v; want stop and shutdown accepted", running.Accepts)
	}
	if st := svcStatusOf(stateStopPending); st.State != svc.StopPending {
		t.Errorf("svcStatusOf(stateStopPending).State = %v; want svc.StopPending", st.State)
	}
}
