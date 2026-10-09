// One service of the host screen. The service's kind decides what its tile offers
// (docs/cli.md#the-app, docs/architecture.md#service-kinds).
import 'package:flutter/material.dart';

import '../app_controller.dart';

/// One service of a host, as a tile.
///
/// The kind decides the buttons: https and http show Open and Use the native app; tcp and udp show the
/// local address and Copy address; unknown shows Copy address and Try opening.
///
/// Copy address copies `127.0.0.1:<port>`, or, when [vpnMode] is on, the service's name
/// `<service>.<host>.internal`, where `<host>` is [hostName] lowercased with dashes
/// (docs/architecture.md#vpn-mode-android-and-ios). A long press on the tile asks for a local port and
/// calls [onSetPort] with the port the user typed.
class ServiceTile extends StatelessWidget {
  const ServiceTile({
    required this.service,
    required this.hostName,
    required this.vpnMode,
    required this.onSetPort,
    this.onOpen,
    super.key,
  });

  /// The service as the view shows it: its name, its kind and the local port bound for it.
  final ServiceView service;

  /// The host's name in the app, from which the VPN name is built.
  final String hostName;

  /// Whether VPN mode is on.
  final bool vpnMode;

  /// Called with the local port the user set by hand. The screen saves it and connects again.
  final Future<void> Function(int port) onSetPort;

  /// Called by Open and by Try opening. Null when the screen has no browser to open the service in.
  final VoidCallback? onOpen;

  @override
  Widget build(BuildContext context) {
    throw UnimplementedError();
  }
}
