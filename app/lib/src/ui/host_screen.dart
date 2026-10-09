// The host screen: the host's route badge and its services as tiles, each with its local port and copy
// address (docs/cli.md#the-app).
import 'package:flutter/material.dart';

import '../app_controller.dart';
import '../store/host_store.dart';

/// Shows one host (docs/cli.md#the-app).
///
/// The route badge shows LAN, Direct or Relay for the route the controller reports; Looking for host...
/// while the route is looking; and Can't reach host, with the host's error code, only when the route is
/// unreachable. The services are tiles, one per service, built from the controller's view of the host.
///
/// The screen follows [controller]. A port set by hand on a tile is saved with [store] (HostStore.savePort),
/// and the screen then connects the host again, so the new port is sent with the connect.
class HostScreen extends StatelessWidget {
  const HostScreen({
    required this.controller,
    required this.store,
    required this.hostId,
    this.vpnMode = false,
    super.key,
  });

  /// The controller that drives the engine and holds the host's view.
  final AppController controller;

  /// The store that holds the host and its remembered ports.
  final HostStore store;

  /// The id of the host the screen shows.
  final String hostId;

  /// Whether VPN mode is on. Copy address then copies the service's name instead of `127.0.0.1:<port>`.
  final bool vpnMode;

  @override
  Widget build(BuildContext context) {
    throw UnimplementedError();
  }
}
