# Running a relay on a VPS

A relay is an optional server that you run for your own hosts and apps. It is needed only when both
ends sit on networks that cannot hole-punch, such as mobile data on both sides
([relay route](architecture.md#relay-route)). It forwards the encrypted session and cannot read it.

This guide walks through a relay on a virtual private server (VPS). The commands are those in
[Running a relay](cli.md#running-a-relay) and [Commands](cli.md#commands). The examples use
documentation addresses and names: `192.0.2.10` stands for the server's public address and
`relay.example.com` for its name. Replace them with your own.

## What you need

- A server with a **public IPv4 address**. Avoid a machine behind a home router or carrier NAT: if its
  NAT changes ports, the relay reports `randomized=true`, which means it cannot be a relay
  ([step 3](#3-start-the-relay)).
- **Inbound UDP** reachable from the internet ([step 1](#1-open-udp-on-the-server)).
- The `holebridge` binary, installed as on the host ([Install](cli.md#install)).
- The deployment's **application key** (`app.key`), copied from the host. The relay must use the same
  one, and it is a secret ([security](security.md#the-application-key)).

## 1. Open UDP on the server

The relay asks the operating system for its UDP port when it starts, and the CLI has no port option
yet. The port can therefore change between runs, and the firewall rule has to allow inbound UDP to
the relay. On a server used only for the relay, allow inbound UDP in the provider's firewall and in
the server's own firewall. Keep SSH and any other service on their own rules.

## 2. Create the keys

Create the relay key on the server:

```sh
holebridge relay --new-key
```

It prints the key once, with the path it was saved to:

```
Relay key: XXX-XXX-XXX   (saved to ~/.config/holebridge/relay.key, mode 0600)
```

`relay --new-key` refuses to overwrite an existing `relay.key`. Keep the key somewhere safe: you
need the same key on each host and in each app ([step 4](#4-put-the-relay-key-on-your-hosts-and-apps)).

Then put the application key in the relay's config directory, as `app.key`:

- copy `app.key` from the host's config directory over an encrypted channel, such as `scp`; or
- print the key on the host with `holebridge app-key`, and set it on the server with
  `holebridge app-key --set <hex>`.

Do not let the server create its own application key. A new key belongs to a new deployment, and a
relay that runs under it admits none of your hosts or apps. Put the key in place before anything
runs on a fresh server that might create one: the key is created at install or first run
([Commands](cli.md#commands)).

Keep both key files private:

```sh
chmod 600 ~/.config/holebridge/relay.key ~/.config/holebridge/app.key
```

The relay refuses an `app.key` that other users can read ([HB-APPKEY-PERMS](errors.md#hb-appkey-perms))
and a `relay.key` that they can read ([HB-RELAY-KEY-PERMS](errors.md#hb-relay-key-perms)).

## 3. Start the relay

```sh
holebridge relay
```

The relay runs in the foreground until it is interrupted. It prints two lines once it has started:

```
Relay public key <hex>
Public UDP <host>:<port> firewalled=<bool> randomized=<bool>
```

- `firewalled=true` means the UDP port is not reachable from outside yet. Check the firewall rules in
  [step 1](#1-open-udp-on-the-server).
- `randomized=true` means the server is behind a NAT that changes its ports, so it cannot be a
  relay. Use a server with a public address.
- The address can show no host, `:<port>`, until peers report the server's address. Wait a few
  minutes.

The relay logs to standard error. Every 10 minutes it logs how many pairings it has matched. It never
logs a key.

Start-at-boot units come with the M6 packaging ([Install](cli.md#install)). Until then, run the relay
under a supervisor that you set up, such as a systemd unit that you write. Point it at the config
directory that holds `relay.key` and `app.key`, with the global `--config` option.

## 4. Put the relay key on your hosts and apps

Use the same relay key everywhere:

- **Host:** add the key as the `relay` field in `host.json`, then restart `holebridge host`. The file
  holds the host key, so keep it at mode `0600` ([host.json](cli.md#configuration)).
- **App:** open **Settings > Relay** and enter the key. It is needed only when both ends are on
  networks that cannot hole-punch ([Settings](cli.md#the-app)).

If only one side has the key, or the two keys differ, the relay turns the other side away
([HB-RELAY-REFUSED](errors.md#hb-relay-refused)).

## 5. Check the relay

Run the check on a machine that holds the same `app.key` as the relay, normally the host. The check
needs the relay key in a file on that machine. Copy `relay.key` over an encrypted channel and keep its
mode at `0600`:

```sh
holebridge relay check path/to/relay.key
```

A working relay prints:

```
member: admitted
stranger: refused
```

and exits with `0`. The two lines mean:

- **member: admitted** is a machine with your app key and relay key getting in. If it prints
  `member: not admitted` the check exits `1` with HB-RELAY-REFUSED. The app keys or relay keys differ;
  compare `app.key` and the relay key on both machines.
- **stranger: refused** is a machine with a random key being turned away. If it prints
  `stranger: admitted`, the check exits `1`: do not use this relay, because it admits keys that are
  not members.
- **stranger: no answer** means the check could not decide. The relay may not be running, or its UDP
  port may not be reachable. Check [step 1](#1-open-udp-on-the-server) and that `holebridge relay` is
  still running.

`--bootstrap <host:port,...>` replaces the public bootstrap nodes that the relay and the check use by
default. Use it only on a private network.

## What the relay sees

| The relay sees | The relay does not see |
|---|---|
| The IP addresses of the hosts and apps that connect | The content of any session: each session is end-to-end encrypted |
| Connection times and data volumes | Service names or targets |
| How many pairings it has matched, in its log | Any key in its log: the relay never logs one |

The server holds `relay.key` and `app.key` on disk, so treat it as a machine with secrets. Anyone with
a relay key can use the relay, but cannot reach a host without that host's key
([security](security.md#relay-keys)).

If a relay key leaks, move `relay.key` aside, run `holebridge relay --new-key`, restart the relay, and
put the new key on each host and in each app. If `app.key` leaks, `holebridge app-key --rotate`
replaces it, and every app must be re-provisioned or rebuilt
([security](security.md#the-application-key)).

## Troubleshooting

| You see | Do this |
|---|---|
| `HB-RELAY-KEY-MISSING` | There is no `relay.key` in the config directory. Create it with `relay --new-key`, or copy it from the place it was saved ([errors](errors.md#hb-relay-key-missing)). |
| `HB-RELAY-KEY-PERMS` | `chmod 600` the relay key file ([errors](errors.md#hb-relay-key-perms)). |
| `HB-APPKEY-MISSING` or `HB-APPKEY-PERMS` | Put `app.key` in the config directory at mode `0600` ([errors](errors.md#hb-appkey-missing)). |
| `firewalled=true` | Open inbound UDP to the relay ([step 1](#1-open-udp-on-the-server)). |
| `randomized=true` | Move the relay to a server with a public address that does not change ports. |
| `member: not admitted`, `HB-RELAY-REFUSED` | Compare the app key and the relay key on the relay and on the check machine ([errors](errors.md#hb-relay-refused)). |
