package cli

import (
	"os"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/config"
	"github.com/andrewloable/HoleBridge/internal/keys"
	"github.com/andrewloable/HoleBridge/internal/status"
	"github.com/andrewloable/HoleBridge/internal/testvec"
	"github.com/andrewloable/HoleBridge/pears/hyperdht"
)

// relayMemberPublicKey returns the member public key of the first relay vector in spec/vectors/key-derivation.json.
func relayMemberPublicKey(t *testing.T) [32]byte {
	t.Helper()
	var v struct {
		RelayKeys []struct {
			PublicKeys map[string]string `json:"publicKeys"`
		} `json:"relayKeys"`
	}
	testvec.Load(t, "key-derivation.json", &v)
	if len(v.RelayKeys) == 0 {
		t.Fatal("key-derivation.json has no relayKeys")
	}
	b := testvec.Hex(t, v.RelayKeys[0].PublicKeys["member"])
	if len(b) != 32 {
		t.Fatalf("vector member public key is %d bytes, want 32", len(b))
	}
	var pub [32]byte
	copy(pub[:], b)
	return pub
}

// The DHT node a host starts on takes the member key pair of its relay as the default key pair, and only when host.json
// has a relay (docs/architecture.md, Relay route). hostDHTConfig builds that configuration without starting the node.
func TestHostDHTConfigUsesTheRelayMemberPairOnlyWithARelay(t *testing.T) {
	relayKey, appKey, _ := vectors(t)
	member := relayMemberPublicKey(t)
	bootstrap := []string{"192.0.2.1:49737"}

	withRelay, err := hostDHTConfig(&config.Config{Relay: relayKey}, appKey, bootstrap)
	if err != nil {
		t.Fatalf("hostDHTConfig with a relay: %v", err)
	}
	if withRelay.DefaultKeyPair == nil || withRelay.DefaultKeyPair.Public != member {
		t.Fatal("with a relay the DHT default key pair is not the member key pair of the relay")
	}
	if !slices.Equal(withRelay.Bootstrap, bootstrap) {
		t.Fatalf("bootstrap nodes = %q, want %q", withRelay.Bootstrap, bootstrap)
	}

	withoutRelay, err := hostDHTConfig(&config.Config{}, appKey, bootstrap)
	if err != nil {
		t.Fatalf("hostDHTConfig without a relay: %v", err)
	}
	if withoutRelay.DefaultKeyPair != nil {
		t.Fatal("without a relay the DHT default key pair must stay as the DHT makes it")
	}
	if !slices.Equal(withoutRelay.Bootstrap, bootstrap) {
		t.Fatalf("bootstrap nodes without a relay = %q, want %q", withoutRelay.Bootstrap, bootstrap)
	}
}

// reloadAddingSSH saves cfg with the service ssh added, sends SIGHUP to this process, which runs the host of dir, and
// waits until the status answer lists ssh. The reload has then applied host.json. It fails the test when no such answer
// comes within 10 s.
func reloadAddingSSH(t *testing.T, dir string, cfg *config.Config) {
	t.Helper()
	if cfg.Services == nil {
		cfg.Services = map[string]config.Service{}
	}
	cfg.Services["ssh"] = config.Service{Target: "127.0.0.1:22"}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("signal SIGHUP: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := queryStatus(dir)
		if err == nil && slices.ContainsFunc(st.Services, func(s status.Service) bool { return s.Name == "ssh" }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGHUP did not reload the service ssh into the status answer (last error %v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Status reports the relay the host started with. A host that started with no relay still reports none after a reload
// adds one: the DHT node's default key pair and the server's relay policy were set at start, so the new relay is not
// in effect (docs/cli.md, status).
func TestStatusRelayStaysNoneAfterAReloadAddsOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGHUP is not delivered to a process on Windows")
	}
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	dir := shortDir(t)
	// LAN off: these tests do not probe the LAN route, and the default LAN ports may be held elsewhere.
	writeHostJSON(t, dir, `{"key":"7KQ-M4X-9TR","lan":{"enabled":false}}`)

	done, stop := startHost(t, dir)
	if st := waitForControl(t, dir, done); st.Relay {
		t.Fatal("a host with no relay in host.json reports one before any reload")
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Relay = keys.Generate()
	reloadAddingSSH(t, dir, cfg)

	st, err := queryStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Relay {
		t.Error("after a reload that adds a relay, status reports one: status must report the relay the host started with")
	}
	stop()
	if d := <-done; d.code != 0 {
		t.Errorf("host exit code = %d, want 0 (stderr %q)", d.code, d.stderr)
	}
}

// Status reports the relay the host started with. A host that started with a relay still reports one after a reload
// removes it: the host keeps dialling that relay under its member key until a restart (docs/cli.md, status).
func TestStatusRelayStaysSetAfterAReloadRemovesIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGHUP is not delivered to a process on Windows")
	}
	tn := hyperdht.NewTestnet(t, testnetSize)
	useTestnet(t, tn)
	dir := shortDir(t)
	writeHostJSON(t, dir, `{"key":"7KQ-M4X-9TR","relay":"`+keys.Format(keys.Generate())+`","lan":{"enabled":false}}`)

	done, stop := startHost(t, dir)
	if st := waitForControl(t, dir, done); !st.Relay {
		t.Fatal("a host with a relay in host.json reports none before any reload")
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Relay = ""
	reloadAddingSSH(t, dir, cfg)

	st, err := queryStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Relay {
		t.Error("after a reload that removes the relay, status reports none: status must report the relay the host started with")
	}
	stop()
	if d := <-done; d.code != 0 {
		t.Errorf("host exit code = %d, want 0 (stderr %q)", d.code, d.stderr)
	}
}
