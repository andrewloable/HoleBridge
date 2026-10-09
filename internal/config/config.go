// Package config reads and writes host.json and finds the config directory. host.json holds the
// host key, a secret: it is written with mode 0600, and on POSIX systems Load refuses a file that
// group or others can read (docs/security.md, rules for implementers, rule 3).
package config

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/keys"
)

// Default LAN ports. PROVISIONAL: the LAN spike (HoleBridge-dxe) has not chosen them yet. When it
// does, set the final values here and in docs/cli.md (lan.discoveryPort, lan.port).
const (
	defaultDiscoveryPort = 27420 // UDP probe port
	defaultPort          = 27421 // TCP session port
)

// Duration is a time.Duration that marshals to JSON as a string such as "8h" or "60s".
type Duration time.Duration

// MarshalJSON writes d as a quoted duration string, without zero units: "8h", not "8h0m0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return json.Marshal(s)
}

// UnmarshalJSON reads a quoted duration string such as "8h". null leaves d unchanged.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Service is one entry of host.json's services object.
type Service struct {
	Target  string   `json:"target"`
	Kind    string   `json:"kind,omitempty"`
	Origins []string `json:"origins,omitempty"`
	Idle    Duration `json:"idle,omitempty"`
}

// LANConfig is host.json's lan object. Load fills in the defaults: Enabled true and the two
// provisional ports (see defaultDiscoveryPort).
type LANConfig struct {
	Enabled       *bool `json:"enabled,omitempty"`
	DiscoveryPort int   `json:"discoveryPort,omitempty"`
	Port          int   `json:"port,omitempty"`
}

// Limits are the resource caps of docs/architecture.md#limits. Sizes are byte counts; the
// durations are strings such as "10s" in JSON.
type Limits struct {
	SessionsPerKey         int      `json:"sessionsPerKey"`
	StreamsPerSession      int      `json:"streamsPerSession"`
	StreamsTotal           int      `json:"streamsTotal"`
	TargetConnectTimeout   Duration `json:"targetConnectTimeout"`
	LANHandshakeDeadline   Duration `json:"lanHandshakeDeadline"`
	UnauthLANTotal         int      `json:"unauthLANTotal"`
	UnauthLANPerIP         int      `json:"unauthLANPerIP"`
	ReceiveWindowPerStream int      `json:"receiveWindowPerStream"`
	UDPFlowsPerSession     int      `json:"udpFlowsPerSession"`
	UDPFlowsTotal          int      `json:"udpFlowsTotal"`
	UDPFlowIdle            Duration `json:"udpFlowIdle"`
	MaxDatagram            int      `json:"maxDatagram"`
	OrderedDatagramQueue   int      `json:"orderedDatagramQueue"`
	ReceiveBudget          int      `json:"receiveBudget"`
}

// Config is host.json. Key and Relay hold the canonical 9-symbol form; the file shows them as
// XXX-XXX-XXX.
type Config struct {
	Key      string             `json:"key,omitempty"`
	Services map[string]Service `json:"services"`
	LAN      LANConfig          `json:"lan"`
	Relay    string             `json:"relay,omitempty"`
	Limits   Limits             `json:"limits"`
}

// defaultLAN is the lan object a file without one gets: enabled, and the two provisional ports.
func defaultLAN() LANConfig {
	on := true
	return LANConfig{Enabled: &on, DiscoveryPort: defaultDiscoveryPort, Port: defaultPort}
}

// defaultLimits are the defaults of docs/architecture.md#limits.
func defaultLimits() Limits {
	return Limits{
		SessionsPerKey:         32,
		StreamsPerSession:      128,
		StreamsTotal:           1024,
		TargetConnectTimeout:   Duration(10 * time.Second),
		LANHandshakeDeadline:   Duration(5 * time.Second),
		UnauthLANTotal:         32,
		UnauthLANPerIP:         4,
		ReceiveWindowPerStream: 2 << 20,
		UDPFlowsPerSession:     256,
		UDPFlowsTotal:          4096,
		UDPFlowIdle:            Duration(60 * time.Second),
		MaxDatagram:            1144, // 1156-byte unordered message less the 12-byte frame header: docs/spike-m1.md, "Unordered datagrams"
		OrderedDatagramQueue:   256 << 10,
		ReceiveBudget:          256 << 20,
	}
}

// Defaults returns the config of a host that has no host.json: no key, no services, and the lan and limits
// defaults. The share command builds its config with it, since it keeps no host.json.
func Defaults() *Config {
	return &Config{Services: map[string]Service{}, LAN: defaultLAN(), Limits: defaultLimits()}
}

// Dir returns the config directory: flag if set, else HOLEBRIDGE_CONFIG, else
// $XDG_CONFIG_HOME/holebridge, else ~/.config/holebridge (%APPDATA%\holebridge on Windows).
func Dir(flag string, getenv func(string) string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := getenv("HOLEBRIDGE_CONFIG"); v != "" {
		return v, nil
	}
	if v := getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "holebridge"), nil
	}
	if runtime.GOOS == "windows" {
		if v := getenv("APPDATA"); v != "" {
			return filepath.Join(v, "holebridge"), nil
		}
	} else if v := getenv("HOME"); v != "" {
		return filepath.Join(v, ".config", "holebridge"), nil
	}
	return "", invalid("no config directory: set HOLEBRIDGE_CONFIG or pass --config")
}

// Load reads dir/host.json, validates it and returns it with the defaults filled in. Service
// names, targets, kinds and origins are checked; an invalid file returns HB-CONFIG-INVALID. On
// POSIX systems a file that group or others can read returns HB-CONFIG-PERMS.
func Load(dir string) (*Config, error) {
	f, err := os.Open(filepath.Join(dir, "host.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if runtime.GOOS != "windows" {
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, errs.E("HB-CONFIG-PERMS", "", nil)
		}
	}

	// Defaults first: the file overrides only the fields it names. The decoder error is not
	// wrapped, because its text can quote part of the file, which holds the key.
	c := Config{LAN: defaultLAN(), Limits: defaultLimits()}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	bad := "host.json is not valid JSON or has an unknown field"
	if err := dec.Decode(&c); err != nil {
		return nil, invalid(bad)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalid(bad)
	}
	if c.Services == nil {
		c.Services = map[string]Service{}
	}
	if c.Key, err = normalizeKey(c.Key, "key"); err != nil {
		return nil, err
	}
	if c.Relay, err = normalizeKey(c.Relay, "relay key"); err != nil {
		return nil, err
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	return &c, nil
}

// hostFile is what Save writes: a Config whose lan and limits objects hold only the fields sparse
// keeps.
type hostFile struct {
	Key      string                     `json:"key,omitempty"`
	Services map[string]Service         `json:"services"`
	LAN      map[string]json.RawMessage `json:"lan"`
	Relay    string                     `json:"relay,omitempty"`
	Limits   map[string]json.RawMessage `json:"limits"`
}

// Save writes c to dir/host.json with mode 0600, JSON indented by two spaces, through an atomic
// rename. It refuses a config that Load would reject.
//
// Save writes only the defaults the owner has moved away from. A LAN or limits field equal to its
// default is left out, unless host.json already holds that key, so a later release that changes a
// default reaches every host that never set the field. A LAN port of zero is not set and is left
// out too.
func Save(dir string, c *Config) error {
	probe := *c
	if probe.LAN.DiscoveryPort == 0 {
		probe.LAN.DiscoveryPort = defaultDiscoveryPort
	}
	if probe.LAN.Port == 0 {
		probe.LAN.Port = defaultPort
	}
	if err := probe.check(); err != nil {
		return err
	}
	keep := onDisk(dir)
	lan, err := sparse("lan.", c.LAN, defaultLAN(), keep)
	if err != nil {
		return err
	}
	limits, err := sparse("limits.", c.Limits, defaultLimits(), keep)
	if err != nil {
		return err
	}
	out := hostFile{
		Key:      keys.Format(c.Key),
		Services: c.Services,
		LAN:      lan,
		Relay:    keys.Format(c.Relay),
		Limits:   limits,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// CreateTemp makes the file mode 0600; the rename keeps it.
	tmp, err := os.CreateTemp(dir, ".host.json-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has moved the file
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "host.json"))
}

// onDisk returns the lan and limits keys that dir/host.json holds now, as "lan.<name>" and
// "limits.<name>". A missing or unreadable file holds none.
func onDisk(dir string) map[string]bool {
	b, err := os.ReadFile(filepath.Join(dir, "host.json"))
	if err != nil {
		return nil
	}
	var top struct {
		LAN    map[string]json.RawMessage `json:"lan"`
		Limits map[string]json.RawMessage `json:"limits"`
	}
	if json.Unmarshal(b, &top) != nil {
		return nil
	}
	keep := map[string]bool{}
	for name := range top.LAN {
		keep["lan."+name] = true
	}
	for name := range top.Limits {
		keep["limits."+name] = true
	}
	return keep
}

// sparse returns the fields of v that Save writes: those that differ from their value in def, and
// those named in keep. prefix ("lan." or "limits.") is how keep names them.
func sparse(prefix string, v, def any, keep map[string]bool) (map[string]json.RawMessage, error) {
	got, err := fieldsOf(v)
	if err != nil {
		return nil, err
	}
	want, err := fieldsOf(def)
	if err != nil {
		return nil, err
	}
	for name, raw := range got {
		if bytes.Equal(raw, want[name]) && !keep[prefix+name] {
			delete(got, name)
		}
	}
	return got, nil
}

// fieldsOf returns the JSON object of v as a map from field name to its JSON value.
func fieldsOf(v any) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ParseTarget splits a target as docs/cli.md writes it. "8080" means 127.0.0.1:8080; host:port
// and [IPv6]:port are split as net.SplitHostPort does, without brackets around the IPv6 host.
func ParseTarget(s string) (host string, port int, err error) {
	host, p := "127.0.0.1", s
	if strings.Contains(s, ":") {
		if host, p, err = net.SplitHostPort(s); err != nil || host == "" {
			return "", 0, invalid("target")
		}
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return "", 0, invalid("target")
	}
	return host, int(n), nil
}

// ValidServiceName reports whether s is a service name: 1 to 32 characters from a-z, 0-9 and
// dash, starting with a letter or digit.
func ValidServiceName(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// check reports whether c is valid for Save and for Load after Load has normalized the keys.
// Error details name the field, never a service name, address or key value.
func (c *Config) check() error {
	if !canonicalKey(c.Key) {
		return invalid("key is not a valid key")
	}
	if !canonicalKey(c.Relay) {
		return invalid("relay key is not a valid key")
	}
	for name, s := range c.Services {
		if !ValidServiceName(name) {
			return invalid("service name")
		}
		if _, _, err := ParseTarget(s.Target); err != nil {
			return invalid("service target")
		}
		switch s.Kind {
		case "", "http", "https":
		case "tcp", "udp":
			if len(s.Origins) > 0 {
				return invalid("origins need a web kind (http or https)")
			}
		default:
			return invalid("service kind")
		}
		if s.Idle < 0 {
			return invalid("service idle")
		}
	}
	for _, p := range []int{c.LAN.DiscoveryPort, c.LAN.Port} {
		if p < 1 || p > 65535 {
			return invalid("LAN port")
		}
	}
	return nil
}

// normalizeKey returns the canonical form of a key read from host.json. An empty key stays empty.
func normalizeKey(s, field string) (string, error) {
	if s == "" {
		return "", nil
	}
	k, err := keys.Normalize(s)
	if err != nil {
		return "", invalid(field + " is not a valid key")
	}
	return k, nil
}

// canonicalKey reports whether s is empty or already in the canonical 9-symbol form.
func canonicalKey(s string) bool {
	if s == "" {
		return true
	}
	k, err := keys.Normalize(s)
	return err == nil && k == s
}

// invalid returns a HB-CONFIG-INVALID error whose detail names the bad field.
func invalid(detail string) error {
	return errs.E("HB-CONFIG-INVALID", detail, nil)
}
