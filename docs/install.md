# Installing the host at boot

How to run `holebridge host` as a service that starts at boot: a systemd unit on Linux and a launchd
LaunchDaemon on macOS. The commands, the config directory and the `host` command are in
[cli.md](cli.md). The security rules the service follows are in [security.md](security.md).

Both services run the host as a dedicated, unprivileged account with its own config directory. Neither
one holds a key: the keys live in files in that directory.

The binary goes in `/usr/local/bin/holebridge`. Both units expect it there. The install script
([cli.md, install](cli.md#install)) is meant to put it there, but it is not implemented yet. Until it
is, copy the binary by hand:

```sh
sudo install -m 0755 holebridge /usr/local/bin/holebridge
holebridge --version
```

If the binary is elsewhere, change `ExecStart` in the unit or `ProgramArguments` in the plist.

Run every `holebridge` command that writes to the config directory as the service user (`sudo -u`,
below). A file that root creates there is not readable by the service, and the host then fails to
start.

## Linux (systemd)

The unit is [`packaging/systemd/holebridge.service`](../packaging/systemd/holebridge.service). It runs
`holebridge host` as the `holebridge` user with `HOLEBRIDGE_CONFIG=/var/lib/holebridge`. systemd creates
that directory as the `StateDirectory`, with mode 0700 and owned by `holebridge`.

1. Create the user and its config directory:

   ```sh
   sudo useradd --system --user-group --no-create-home --home-dir /var/lib/holebridge --shell /usr/sbin/nologin holebridge
   sudo install -d -m 0700 -o holebridge -g holebridge /var/lib/holebridge
   ```

2. Add the services, as the service user:

   ```sh
   sudo -u holebridge /usr/local/bin/holebridge --config /var/lib/holebridge service add web 127.0.0.1:8080
   ```

3. Install the unit, start it and enable it for boot:

   ```sh
   sudo install -m 0644 packaging/systemd/holebridge.service /etc/systemd/system/holebridge.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now holebridge
   systemctl status holebridge
   ```

   `enable` starts the host at every boot. `Restart=on-failure` restarts it after a failure.

4. Read the host's key and its link. This is how you pair an app, since the service does not print
   them (see [Keys and the banner](#keys-and-the-banner)):

   ```sh
   sudo -u holebridge /usr/local/bin/holebridge --config /var/lib/holebridge key
   ```

Logs go to the journal: `journalctl -u holebridge`. After an upgrade of the binary, run
`sudo systemctl restart holebridge`. To stop it and keep it from starting at boot, run
`sudo systemctl disable --now holebridge`.

## macOS (launchd)

The plist is [`packaging/launchd/holebridge.plist`](../packaging/launchd/holebridge.plist). It is a
LaunchDaemon with the label `app.holebridge.host`. It runs `holebridge host` as the `_holebridge`
user with `HOLEBRIDGE_CONFIG=/var/db/holebridge`.

1. Create the account. Pick IDs below 500 that no account uses (list them with
   `dscl . -list /Users UniqueID`); 301 is an example:

   ```sh
   sudo dscl . -create /Groups/_holebridge
   sudo dscl . -create /Groups/_holebridge PrimaryGroupID 301
   sudo dscl . -create /Users/_holebridge
   sudo dscl . -create /Users/_holebridge UniqueID 301
   sudo dscl . -create /Users/_holebridge PrimaryGroupID 301
   sudo dscl . -create /Users/_holebridge UserShell /usr/bin/false
   sudo dscl . -create /Users/_holebridge NFSHomeDirectory /var/empty
   sudo dscl . -create /Users/_holebridge RealName "HoleBridge host"
   sudo dscl . -create /Users/_holebridge IsHidden 1
   ```

2. Create the config directory:

   ```sh
   sudo install -d -m 0700 -o _holebridge -g _holebridge /var/db/holebridge
   ```

3. Add the services, as the service user:

   ```sh
   sudo -u _holebridge /usr/local/bin/holebridge --config /var/db/holebridge service add web 127.0.0.1:8080
   ```

4. Install the plist and load it. It must be owned by `root:wheel`, or launchd refuses to load it:

   ```sh
   sudo install -o root -g wheel -m 0644 packaging/launchd/holebridge.plist /Library/LaunchDaemons/app.holebridge.host.plist
   sudo launchctl bootstrap system /Library/LaunchDaemons/app.holebridge.host.plist
   sudo launchctl print system/app.holebridge.host
   ```

   A plist in `/Library/LaunchDaemons` loads at every boot (`RunAtLoad`). `KeepAlive` restarts the host
   after an unsuccessful exit.

5. Read the host's key and its link:

   ```sh
   sudo -u _holebridge /usr/local/bin/holebridge --config /var/db/holebridge key
   ```

The log is `/var/log/holebridge.log`. After an upgrade of the binary, run
`sudo launchctl kickstart -k system/app.holebridge.host`. To stop the host until the next boot, run
`sudo launchctl bootout system/app.holebridge.host`. To stop it for good, also delete
`/Library/LaunchDaemons/app.holebridge.host.plist`.

If the macOS application firewall is on, allow `holebridge` to accept incoming connections
(System Settings, Network, Firewall). Otherwise the LAN route cannot reach the host.

## Keys and the banner

The host has two secrets in its config directory: `host.json` holds the host key, and `app.key` holds
the application key ([security.md](security.md#the-key)). Neither is in a unit, a plist, a command line
or the environment. Protect the directory: it must be mode 0700 and owned by the service user, and
the host refuses to start otherwise.

When the host starts in a terminal, it prints a banner with the key, a QR code and a link. The link
carries the application key too. Under the services, both units discard stdout (`StandardOutput=null`
and `StandardOutPath=/dev/null`), so the banner never reaches the journal or a log file. The host has
no option to print the banner somewhere else yet. Use `holebridge key` as in the steps above to get
the same QR code and link on your terminal, and `holebridge app-key` to print the application key by
itself.

The host's log goes to the journal (Linux) or `/var/log/holebridge.log` (macOS). The host must never
log a key ([security.md](security.md#rules-for-implementers)).

Treat the QR code, the link and `app.key` as secrets: share them only with people you trust
([security.md](security.md#the-key)).
