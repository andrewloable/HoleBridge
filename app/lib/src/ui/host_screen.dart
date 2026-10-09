// The host screen: the host's route badge and its services as tiles, each with its local port and copy
// address (docs/cli.md#the-app).
import 'dart:async';

import 'package:flutter/material.dart';

import '../app_controller.dart';
import '../engine/ipc_codec.dart' show IpcDesync;
import '../store/host_store.dart';
import 'service_tile.dart';

/// Shows one host (docs/cli.md#the-app).
///
/// The route badge shows LAN, Direct or Relay for the route the controller reports; Looking for host...
/// while the route is looking; and Can't reach host, with the host's error code, only when the route is
/// unreachable. The services are tiles, one per service, built from the controller's view of the host.
///
/// Opening the screen connects the host (AppController.connect). The screen follows [controller]. A port
/// set by hand on a tile is saved with [store] (HostStore.savePort), and the screen then reconnects the
/// host (AppController.reconnect), so the new port is sent with the connect.
class HostScreen extends StatefulWidget {
  const HostScreen({
    required this.controller,
    required this.store,
    required this.hostId,
    this.vpnMode = false,
    this.onOpenSettings,
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

  /// Runs when the Settings button in the app bar is pressed. Null hides the button.
  final VoidCallback? onOpenSettings;

  @override
  State<HostScreen> createState() => _HostScreenState();
}

const _engineNotice = 'The app engine did not answer. Try again.';

class _HostScreenState extends State<HostScreen> {
  /// The host's name in the app, or empty until the store has been read.
  String _name = '';

  /// Whether the engine did not answer the last connect or reconnect, so the screen says so.
  bool _engineDown = false;

  @override
  void initState() {
    super.initState();
    unawaited(_loadName());
    unawaited(_engine(() => widget.controller.connect(widget.hostId)));
  }

  Future<void> _loadName() async {
    for (final host in await widget.store.hosts()) {
      if (host.id == widget.hostId) {
        if (mounted) setState(() => _name = host.name);
        return;
      }
    }
  }

  /// Runs one engine call of the screen. A call that throws StateError (the engine is not running),
  /// TimeoutException or IpcDesync means the engine did not answer, as in AddHostScreen, and the screen
  /// shows the notice until a call succeeds. Any other error is a bug and goes on up.
  Future<void> _engine(Future<void> Function() call) async {
    var down = false;
    try {
      await call();
    } on StateError {
      down = true;
    } on TimeoutException {
      down = true;
    } on IpcDesync {
      down = true;
    }
    if (mounted && down != _engineDown) setState(() => _engineDown = down);
  }

  /// Saves a port the user set by hand for [service], then reconnects the host so the engine binds it.
  Future<void> _setPort(String service, int port) async {
    await widget.store.savePort(widget.hostId, service, port);
    await _engine(() => widget.controller.reconnect(widget.hostId));
  }

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: widget.controller,
      builder: (context, _) {
        final view = widget.controller.view(widget.hostId);
        return Scaffold(
          appBar: AppBar(
            title: Text(_name),
            actions: [
              if (widget.onOpenSettings != null)
                IconButton(
                  icon: const Icon(Icons.settings),
                  tooltip: 'Settings',
                  onPressed: widget.onOpenSettings,
                ),
            ],
          ),
          body: ListView(
            padding: const EdgeInsets.all(16),
            children: [
              _RouteBadge(route: view.route, errorCode: view.lastErrorCode),
              if (_engineDown) ...[const SizedBox(height: 8), const Text(_engineNotice)],
              const SizedBox(height: 16),
              for (final service in view.services)
                ServiceTile(
                  service: service,
                  hostName: _name,
                  vpnMode: widget.vpnMode,
                  onSetPort: (port) => _setPort(service.name, port),
                ),
            ],
          ),
        );
      },
    );
  }
}

/// The route badge (docs/cli.md#the-app): LAN, Direct or Relay; Looking for host... while a lookup runs;
/// and Can't reach host, with the error code, only while the route is unreachable. Nothing is shown when
/// no session is up and no search runs.
///
/// The error code is not shown while the engine searches again, even though the controller keeps the code
/// of the failed lookup until a route comes up.
class _RouteBadge extends StatelessWidget {
  const _RouteBadge({required this.route, this.errorCode});

  /// lan, direct, relay, looking, unreachable, or empty.
  final String route;

  /// The last error code of the host, or null.
  final String? errorCode;

  @override
  Widget build(BuildContext context) {
    final text = Theme.of(context).textTheme;
    if (route == 'unreachable') {
      final code = errorCode;
      return Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text("Can't reach host", style: text.titleMedium),
          if (code != null) Text(code, style: text.labelLarge),
        ],
      );
    }
    final label = switch (route) {
      'lan' => 'LAN',
      'direct' => 'Direct',
      'relay' => 'Relay',
      'looking' => 'Looking for host...',
      _ => null,
    };
    if (label == null) return const SizedBox.shrink();
    return Text(label, style: text.titleMedium);
  }
}
