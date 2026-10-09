package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"text/tabwriter"

	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/status"
	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// The status command registers itself here, as the other commands do.
func init() {
	commands["status"] = statusCmd
}

// statusCmd is holebridge status. It asks the control socket in the config directory for the
// snapshot of the running host or relay and prints it. With no socket answering it fails with
// HB-NOT-RUNNING.
func statusCmd(args []string, env Env, configDir string) error {
	words, _, err := splitFlags(args)
	if err != nil {
		return err
	}
	if len(words) != 0 {
		return usage("status takes no arguments")
	}
	dir, err := requireDir(configDir, env)
	if err != nil {
		return err
	}
	raw, err := status.Query(dir, "status")
	if errors.Is(err, status.ErrNotRunning) {
		return errs.E("HB-NOT-RUNNING", "", err)
	}
	if err != nil {
		return err
	}
	var st status.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	return printStatus(env.Stdout, st)
}

// printStatus prints the internet state and the relay setting, then the services and the sessions. Each
// session is one row: its route, its streams and flows, and its bytes in and out as plain decimal numbers.
func printStatus(w io.Writer, st status.Status) error {
	fmt.Fprintln(w, natLine(st.NAT))
	fmt.Fprintln(w, relayLine(st.Relay))
	fmt.Fprintln(w)
	if len(st.Services) == 0 {
		fmt.Fprintln(w, "Services: none")
	} else {
		t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(t, "SERVICE\tKIND")
		for _, s := range st.Services {
			fmt.Fprintf(t, "%s\t%s\n", s.Name, s.Kind)
		}
		if err := t.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprintln(w)
	if len(st.Sessions) == 0 {
		fmt.Fprintln(w, "Sessions: none")
		return nil
	}
	t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(t, "ROUTE\tSTREAMS\tFLOWS\tBYTES IN\tBYTES OUT")
	for _, s := range st.Sessions {
		fmt.Fprintf(t, "%s\t%d\t%d\t%d\t%d\n", s.Route, s.Streams, s.Flows, s.BytesIn, s.BytesOut)
	}
	return t.Flush()
}

// natLine says whether the node is reachable from the internet and what kind of NAT it is, from the NAT state
// its peers report.
func natLine(n dhtrpc.NATInfo) string {
	switch {
	case n.Host == "":
		return "Internet: unknown (no peer has reported our address yet)"
	case n.Firewalled:
		return "Internet: not reachable (no ping has reached " + n.Host + " from outside)"
	case n.Randomized:
		return "Internet: reachable at " + n.Host + " (NAT: random, the ports change)"
	}
	return "Internet: reachable at " + net.JoinHostPort(n.Host, strconv.Itoa(n.Port)) + " (NAT: consistent)"
}

// relayLine says whether a relay key is set.
func relayLine(set bool) string {
	if set {
		return "Relay: set"
	}
	return "Relay: none set"
}
