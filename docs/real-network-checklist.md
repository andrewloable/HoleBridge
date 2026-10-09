# Real-network checklist (M2)

The checklist the owner runs by hand on real devices and networks before the M2 release. It covers
every real-network item in the [M2 done-when list](roadmap.md#m2-mvp-every-route). It gives steps and
a place to record each result. It records no results: every table is blank until the owner fills it
in.

## Before you start

**What to record.** For each run: the date, the device and OS version, the network type at each end
(for example "home Wi-Fi", "other Wi-Fi", "mobile data", "hotspot"), the route the badge showed, pass
or fail, and notes. Notes can hold an error code (look it up in [errors.md](errors.md)), a stall time,
or anything odd. Record the build under test too: `holebridge --version` on the host, and the app
version each device shows.

**What never to record,** in this file or in any record: an IP address, a Wi-Fi name, a hostname, a
username, an email address, a location, the 9-symbol key, the relay key or the deployment's application
key. Use the names in the table below. The QR code and the link carry both halves of the key
([cli.md](cli.md#hosting)): treat them as secrets, and do not keep screenshots that show them.

**Devices and networks.**

| Name here | What it is | Used in items |
|---|---|---|
| the host | A computer running `holebridge host`, on the home network | all |
| the relay | A server with a public IPv4 address and open UDP ports, running `holebridge relay` | 3, 4, 5 (relay run) |
| the phone | An Android phone, Android 10 or later | 2, 3, 4, 6, 7, 8 |
| the TV | An Android TV or Google TV box, Android 10 or later, with a remote and no camera | 1, 6, 7, 8 |
| the laptop | A laptop running the desktop app | 5, 8 |
| home network | The host's own network. It must allow device-to-device traffic, so not a guest network with client isolation | 1, 5 (LAN run), 6 |
| another network | A network that is neither the host's nor mobile data (open point 6) | 2, 5 (direct run) |
| mobile data | Cellular data on the phone; on the host and the laptop for the relay run (open point 5) | 3, 4, 5 (relay run) |

**Host setup, once per run.** These commands are from [cli.md](cli.md#hosting). Replace each
`<...>` with your own target and keep the targets out of the records. The steps below use the names
jellyfin, ssh and dns; any web service can stand in for jellyfin.

```sh
holebridge service add jellyfin <jellyfin target> --kind http
holebridge service add ssh <ssh target> --kind tcp
holebridge service add dns <dns resolver target> --kind udp
holebridge host
```

`holebridge host` prints the QR code, the link and a LAN address. Do not copy any of them into a
record.

**Reading the app.** The labels in the steps are the app's own ([cli.md](cli.md#the-app)). The
**route badge** reads LAN, Direct or Relay. "Looking for host" is not a failure. A lookup that runs
without finding the host ends in "Can't reach host" with an error code (at most 60 s, or at once when
the lookup fails at once); record that code.

Item 1 needs the TV to hold the host, and item 6 is how the TV gets one, so run item 6 first when the
TV has no host yet.

## 1. TV on the host's LAN, over the LAN route

**Setup**
- The TV holds the host (run item 6 first if it does not).
- The TV and the host are on the home network.

**Steps**
1. Put the TV on the home network and open HoleBridge.
2. Wait for the host to show as connected.
3. Open the jellyfin tile (**Open**).
4. Read the route badge.

**Expected:** the badge reads LAN and the service loads in the TV's browser.
**Fail:** the badge reads Direct, Relay or Can't reach host. Record the code if one shows.

| Date | Devices and OS versions | Network type (each end) | Route shown | Pass or fail | Notes |
|---|---|---|---|---|---|
|  |  |  |  |  |  |
|  |  |  |  |  |  |

## 2. A phone on another network, directly

**Setup**
- The phone holds the host (added by QR code or link).
- The phone is on another network, not mobile data (open point 6).
- No relay key is set on the phone (Settings > **Relay** empty). Then only a direct connection can
  complete, so a pass shows the direct route.

**Steps**
1. Turn off the phone's connection to the home network and join the other network.
2. Open HoleBridge and wait for the host.
3. Open the jellyfin tile (**Open**).
4. Read the route badge.

**Expected:** the badge reads Direct and the service loads.
**Fail:** Can't reach host, with its error code. Record what the badge showed.

| Date | Devices and OS versions | Network type (each end) | Route shown | Pass or fail | Notes |
|---|---|---|---|---|---|
|  |  |  |  |  |  |
|  |  |  |  |  |  |

## 3. Both on mobile data, through the relay

**Setup**
- The relay runs on the relay server. Run `holebridge relay --new-key` once, then `holebridge relay`.
  Its output should read firewalled=false and randomized=false ([cli.md](cli.md#running-a-relay)).
  Do not start the run until it does.
- The same relay key is in the host's `host.json` (`relay`) and in the phone's Settings > **Relay**.
  Restart `holebridge host` after editing `host.json`; before M3 a restart is needed.
- The host and the phone are both on mobile data (open point 5). The phone's Wi-Fi is off.

**Steps**
1. Check the relay output and restart the host.
2. Open HoleBridge on the phone and wait for the host.
3. Open the jellyfin tile (**Open**).
4. Read the route badge.

**Expected:** the badge reads Relay and the service loads.
**Fail:** Can't reach host, with its error code. If the badge reads Direct, record it. This item needs
the relay route, so do not mark it as a pass.

| Date | Devices and OS versions | Network type (each end) | Route shown | Pass or fail | Notes |
|---|---|---|---|---|---|
|  |  |  |  |  |  |
|  |  |  |  |  |  |

## 4. Wi-Fi to mobile-data switch keeps playback

**Setup**
- The phone holds the host and has the relay key set (Settings > **Relay**), so a route exists after
  the switch. The docs do not fix which route carries the stream after a switch; the badge decides.
- A video is playing in the jellyfin web service, opened in the in-app browser (**Open**).

**Steps**
1. Start on the home network. Open the jellyfin tile and start a video. Read the route badge (LAN
   expected).
2. Turn off Wi-Fi on the phone, so only mobile data is left.
3. Keep the video playing. Note the seconds from the switch until playback continues, and the route
   the badge shows after the switch.
4. Note whether the video continues from the same point, or restarts, or shows an error page.

**Expected:** playback continues from the same point, with no error page. A short stall is recorded in
seconds. The stall limit is open point 4: until the owner sets it, record the seconds and do not mark
the item as passed.

| Date | Devices and OS versions | Network type (each end) | Route shown (before, after) | Pass or fail | Notes (stall in seconds) |
|---|---|---|---|---|---|
|  |  |  |  |  |  |
|  |  |  |  |  |  |

## 5. A DNS query through a udp service, on every route

**Setup**
- The host has the dns service (kind udp), set up under Before you start.
- The laptop holds the host and runs the desktop app. Its terminal has `dig`, which macOS includes.
- Run the steps three times, one per route:
  - LAN: the laptop on the home network.
  - Direct: the laptop on another network, with no relay key.
  - Relay: the laptop and the host on mobile data, with the relay key set on both (as in item 3).

**Steps** (repeat for each route)
1. Set up the network for the route. Open the app, wait for the host and read the route badge.
2. Open the dns tile and copy its address. It reads 127.0.0.1 and a port. Note the port only.
3. In a terminal on the laptop, run:

```sh
dig -p <port> @127.0.0.1 example.com +time=5 +tries=1
```

4. Read the ANSWER SECTION of the output.

**Expected:** the output has an ANSWER SECTION with at least one record, within 5 s, and the badge
shows the route planned for that run.
**Fail:** dig prints no answer, or the badge shows a different route. Record which.

| Date | Devices and OS versions | Network type (each end) | Route planned | Pass or fail | Notes |
|---|---|---|---|---|---|
|  |  |  | LAN |  |  |
|  |  |  | Direct |  |  |
|  |  |  | Relay |  |  |

## 6. A TV gets a host from a phone by handoff

**Setup**
- The phone already holds the host (added by QR code or link).
- The TV and the phone are on the home network with device-to-device traffic allowed. A guest network
  with client isolation blocks the handoff ([architecture](architecture.md#adding-a-host-to-a-tv-handoff)).
- The TV holds no host yet.

**Steps**
1. On the TV, open HoleBridge and choose **Add from phone**. The TV shows a QR code.
2. On the phone, scan the TV's QR code (**Scan QR**) so that its link opens in HoleBridge.
3. On the phone, pick the host to send and confirm.
4. On the TV, wait for the host to appear. The code is valid for 5 minutes.
5. On the TV, open the jellyfin tile (**Open**) and read the route badge.

**Expected:** the host appears on the TV within the 5 minutes, the badge reads LAN and the service loads.
**Fail:** the app shows HB-HANDOFF-UNREACHABLE (check the network setup) or HB-HANDOFF-EXPIRED (start
again at step 1).

| Date | Devices and OS versions | Network type (each end) | Route shown | Pass or fail | Notes |
|---|---|---|---|---|---|
|  |  |  |  |  |  |
|  |  |  |  |  |  |

## 7. VPN mode: services by name, with HoleBridge in the background

**Setup**
- The phone and the TV both hold the host.
- No other VPN app runs on either device, WARP included: only one VPN can run at a time
  ([architecture](architecture.md#vpn-mode-android-and-ios)).
- The TV has the Jellyfin app installed. The phone has an SSH client installed.
- Both apps come from the channel chosen in open point 2.

**Steps**
1. On the phone, turn on **VPN mode** and approve the Android VPN prompt.
2. On the TV, turn on **VPN mode** and approve the prompt.
3. In HoleBridge on each device, tap **Copy address** for jellyfin and for ssh. The names have the form
   service, dot, host name, dot internal. Do not record the names; write them as
   `jellyfin.<host>.internal` and `ssh.<host>.internal`.
4. Leave HoleBridge in the background on both devices: go to the home screen. Do not close the app,
   and do not turn VPN mode off.
5. On the TV, open the Jellyfin app and set its server to the jellyfin name. Browse the library or sign in.
6. On the phone, open the SSH client and connect to the ssh name.
7. Confirm that HoleBridge was not on screen during steps 5 and 6.

**Expected:** both connect by name while HoleBridge is in the background, and the VPN icon stays on.
**Fail:** a name does not resolve, a connection stalls or fails, or VPN mode stops when the app leaves
the screen.

| Date | Devices and OS versions | Network type (each end) | Route shown | Pass or fail | Notes |
|---|---|---|---|---|---|
|  |  |  |  |  |  |
|  |  |  |  |  |  |

## 8. Fresh device: store to first Open in under 2 minutes

Run this once on each of an Android phone, a TV and a laptop.

**Setup**
- Remove HoleBridge and its data from the device before its run, so the device starts with no keys.
- The host is running and its QR code and link are shown on a screen the phone can see, or are at hand
  for the laptop.
- For the TV: the phone holds the host and is on the same network as the TV.
- Use a stopwatch. Record the time as minutes and seconds. The clock's start and stop points are open
  point 3.

**Steps, phone**
1. Start the stopwatch. Open the store page and install HoleBridge.
2. Open the host's link, or scan the QR code. If the link opens the store page, the app is not installed
   yet: after installing, open the link or scan again.
3. Wait for the host to show as connected.
4. Tap **Open** on the jellyfin tile. Stop the stopwatch when the page loads.

**Steps, TV**
1. Start the stopwatch. Install HoleBridge from the TV's store.
2. Open HoleBridge and choose **Add from phone**. Use the phone that already holds the host, as in item 6.
3. Open the jellyfin tile (**Open**). Stop the stopwatch when the page loads.

**Steps, laptop**
1. Start the stopwatch. Install the desktop app from the source chosen in open point 1.
2. On the first screen, choose **Paste link** and paste the host's link.
3. Wait for the host to show as connected. Open the jellyfin tile (**Open**). Stop the stopwatch when the
   page loads.

**Expected:** each device takes under 2:00 from start to the first Open.
**Fail:** 2:00 or more. Record which step took longest.

| Date | Device and OS version | Install source | Elapsed (m:ss) | Pass or fail | Notes |
|---|---|---|---|---|---|
|  |  |  |  |  |  |
|  |  |  |  |  |  |

## Release record

The M2 release needs a recorded pass for every item. Item 5 needs a pass on each of its three routes.

| Item | Section | Passed on (date) | Devices and OS versions | Notes |
|---|---|---|---|---|
| 1 | TV on the host's LAN, LAN route | | | |
| 2 | Phone on another network, direct | | | |
| 3 | Both on mobile data, relay | | | |
| 4 | Wi-Fi to mobile-data switch keeps playback | | | |
| 5 | DNS through a udp service, LAN, direct and relay | | | |
| 6 | TV gets a host by handoff | | | |
| 7 | VPN mode, names by name, HoleBridge in the background | | | |
| 8 | Fresh device, store to first Open under 2 minutes | | | |

## Open points

These are choices the docs have not made. The checklist does not make them; the owner does, before the run.

1. **Laptop install source (item 8).** The item says "from the store" for the laptop, but the docs put
   desktop installers in M6 and name no laptop store for M2. The owner names the source, for example the
   install script, which is part of M2.
2. **Android install channel (items 7 and 8).** If Google Play rejects the `VpnService` declaration (Q9),
   VPN mode ships only in the GitHub Releases APK. The owner names the channel used for each device.
3. **The 2-minute clock (item 8).** The docs do not say where the clock starts and stops. This page
   starts it at the store install and stops it at the first Open, so the re-scan after installing counts.
   The owner confirms or changes this.
4. **Stall limit (item 4).** The roadmap gives no number. The owner sets the longest acceptable stall
   before the run.
5. **Mobile data for the relay run (items 3 and 5).** The docs describe a host on carrier NAT but not how
   to put the host on mobile data, for example a phone hotspot with the host tethered to it. The owner
   chooses the setup.
6. **Network for the direct run (item 2).** The page asks for another network that is not mobile data,
   because mobile data may not hole-punch ([relay route](architecture.md#relay-route)). The owner may
   choose another network.
7. **Forcing a route (all items).** The docs have no switch to force a route. This page sets the route by
   where the devices are and reads the badge. The owner decides whether a test-only switch is needed.
