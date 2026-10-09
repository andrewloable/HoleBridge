# Error codes

Generated from spec/errors.json by go generate ./internal/errs. Do not edit by hand.

## HB-APPKEY-INVALID

**Problem:** the application key is not valid

**Cause:** An application key is 64 lowercase hex digits. A copied key may be cut short or use capital letters.

**Fix:** Scan the host's QR code again, or copy the application key again from holebridge app-key on the host.

## HB-APPKEY-MISSING

**Problem:** there is no application key for this deployment

**Cause:** Each deployment has one application key. It is made at install or first run, and an app gets it from a host's QR code or link.

**Fix:** On the host, run holebridge app-key --new to create one. In the app, scan the host's QR code or open its link.

## HB-APPKEY-PERMS

**Problem:** app.key can be read by other users

**Cause:** The file holds the application key, a secret for the whole deployment. The host and the relay will not start while others can read it.

**Fix:** chmod 600 ~/.config/holebridge/app.key

## HB-CONFIG-DIR-PERMS

**Problem:** the config directory is not owner-only

**Cause:** The control socket lives in the config directory. The host and the relay do not serve while other users can enter it, or while another user owns it.

**Fix:** chmod 700 ~/.config/holebridge

## HB-CONFIG-INVALID

**Problem:** the configuration is not valid

**Cause:** A value in host.json is missing or wrong, or no config directory is set. The detail names what is wrong.

**Fix:** Fix the value the detail names, in host.json, or set --config or HOLEBRIDGE_CONFIG.

## HB-CONFIG-PERMS

**Problem:** host.json can be read by other users

**Cause:** The file holds the host key.

**Fix:** chmod 600 ~/.config/holebridge/host.json

## HB-HANDOFF-EXPIRED

**Problem:** the pairing code timed out

**Cause:** A pairing code works for 5 minutes, so an old code is not accepted.

**Fix:** On the TV, choose Add from phone again to show a new code.

## HB-HANDOFF-REFUSED

**Problem:** the TV did not accept the host

**Cause:** A TV accepts a host only from a phone that scanned the code it is showing now. The code on the phone is old, or it came from another TV.

**Fix:** On the TV, choose Add from phone to show a new code, then scan that code with the phone.

## HB-HANDOFF-UNREACHABLE

**Problem:** the phone cannot reach the TV

**Cause:** The phone and the TV are not on the same network, or the network keeps devices apart. Guest networks often do.

**Fix:** Use the same network, not a guest network with client isolation.

## HB-IPC-DESYNC

**Problem:** a malformed frame arrived on the worklet's stdout

**Cause:** Native code can still write to fd 1 directly.

**Fix:** The app restarts its engine on its own. If this keeps happening, report it with this code.

## HB-KEY-INVALID

**Problem:** the key is not valid

**Cause:** A key is 9 symbols from 0 to 9 and A to Z, except U. Case, dashes and spaces do not matter.

**Fix:** Type the key shown by holebridge key on the host, or scan its QR code.

## HB-LAN-PORT-IN-USE

**Problem:** a LAN port is already taken

**Cause:** Another program, or a second host on this machine, is using the port.

**Fix:** Stop the other program, or set other lan.port and lan.discoveryPort values in host.json.

## HB-LIMIT-REACHED

**Problem:** the host is at one of its limits

**Cause:** Too many connections or streams are open at once. The host caps them so that no one client can use up its resources.

**Fix:** Close some connections and try again. The host owner can raise a limit under limits in host.json.

## HB-LOOKUP-TIMEOUT

**Problem:** the app could not reach the host

**Cause:** No host answered for this key. The host may be off, the key may be mistyped or from another deployment, or the network may block the connection.

**Fix:** Check that the host is running and that the key is right. The app keeps trying on its own.

## HB-NOT-RUNNING

**Problem:** holebridge is not running

**Cause:** holebridge status reads the control socket of a running host or relay in this config directory. No socket answers, so none is running here.

**Fix:** Start one with holebridge host or holebridge relay, or pass --config for the directory of the one that runs.

## HB-RELAY-KEY-MISSING

**Problem:** there is no relay key in the config directory

**Cause:** A relay runs under the relay key in relay.key. The hosts and apps that use the relay need the same key.

**Fix:** Run holebridge relay --new-key to create relay.key, then put the same key in host.json (relay) and in the app under Settings, Relay.

## HB-RELAY-KEY-PERMS

**Problem:** relay.key can be read by other users

**Cause:** The file holds the relay key, and anyone who has it can use the relay. The relay and relay check do not run while others can read it.

**Fix:** chmod 600 ~/.config/holebridge/relay.key, or the file given to relay check

## HB-RELAY-REFUSED

**Problem:** the relay did not accept this key

**Cause:** Only one side has the relay key, or the two sides have different relay keys.

**Fix:** Put the same relay key in host.json (relay) and in the app under Settings, Relay.

## HB-TARGET-REFUSED

**Problem:** the service refused the connection

**Cause:** Nothing is listening at the service's target, or the service turned the connection down.

**Fix:** Start the service, or correct its target in host.json.

## HB-TARGET-TIMEOUT

**Problem:** the service did not answer in time

**Cause:** The host could not connect to the service within 10 seconds. The service may be busy or stopped, or its target may be wrong.

**Fix:** Check that the service is running and that its target in host.json is right.

## HB-UDP-TOO-LARGE

**Problem:** a datagram is larger than maxDatagram (1144 bytes by default)

**Cause:** A UDP datagram must fit in one encrypted packet. Larger ones are dropped, because HoleBridge does not split them.

**Fix:** Make the application send smaller UDP packets.

## HB-UNKNOWN-SERVICE

**Problem:** the host has no service with this name

**Cause:** The name is not in the host's service list, or the app's copy of the list is out of date.

**Fix:** Check the name with holebridge service ls on the host, then reconnect the app.

## HB-USAGE

**Problem:** the command line is not valid

**Cause:** A command, option or value is missing or not known.

**Fix:** Run holebridge --help to see the commands and options.

## HB-VERSION-MISMATCH

**Problem:** the app and the host speak different protocol versions

**Cause:** The app and the host run different protocol versions, which may not work together.

**Fix:** Update the app from its store, or re-run the install script on the host.
