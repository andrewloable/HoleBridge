// One service of the host screen. The service's kind decides what its tile offers
// (docs/cli.md#the-app, docs/architecture.md#service-kinds).
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../app_controller.dart';

/// One service of a host, as a tile.
///
/// The kind decides the buttons: https and http show Open and Use the native app; tcp and udp show the
/// local address and Copy address; unknown shows Copy address and Try opening.
///
/// Copy address copies `127.0.0.1:<port>`, or, when [vpnMode] is on, the service's name
/// `<service>.<host>.internal`, where `<host>` is [hostName] lowercased with dashes
/// (docs/architecture.md#vpn-mode-android-and-ios). A tap (Select on the remote) or a long press on the
/// tile's header asks for a local port and calls [onSetPort] with the port the user typed.
///
/// [onOpen] is null when the app has no browser to open the service in. Open and Try opening are then
/// shown but disabled.
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

  /// Called with the local port the user set by hand. The screen saves it and reconnects the host.
  final Future<void> Function(int port) onSetPort;

  /// Called by Open and by Try opening. Null when the screen has no browser to open the service in.
  final VoidCallback? onOpen;

  /// The address that Copy address copies: the VPN name with VPN mode on, else `127.0.0.1:<port>`. Null
  /// without VPN mode while no local port is bound.
  String? get _address {
    if (vpnMode) return '${service.name}.${_vpnHostLabel(hostName)}.internal';
    final port = service.port;
    return port == null ? null : '127.0.0.1:$port';
  }

  Future<void> _askPort(BuildContext context) async {
    final port = await showDialog<int>(
      context: context,
      builder: (context) => _PortDialog(service: service.name),
    );
    if (port != null) await onSetPort(port);
  }

  @override
  Widget build(BuildContext context) {
    final text = Theme.of(context).textTheme;
    final address = _address;
    final web = service.kind == 'https' || service.kind == 'http';
    final copy = address == null ? null : () => Clipboard.setData(ClipboardData(text: address));
    // The header is the tile's only tap target. The buttons sit beside it, not inside it: a button inside a
    // focusable tile cannot be reached with the arrows, so the remote could not press it.
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            InkWell(
              onTap: () => _askPort(context),
              onLongPress: () => _askPort(context),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Row(
                    children: [
                      Expanded(child: Text(service.name, style: text.titleMedium)),
                      Text(service.kind, style: text.labelMedium),
                    ],
                  ),
                  if (!web)
                    Padding(
                      padding: const EdgeInsets.only(top: 4),
                      child: Text(address ?? 'Not connected yet'),
                    ),
                ],
              ),
            ),
            Wrap(
              spacing: 8,
              children: web
                  ? [
                      TextButton(onPressed: onOpen, child: const Text('Open')),
                      TextButton(onPressed: copy, child: const Text('Use the native app')),
                    ]
                  : [
                      TextButton(onPressed: copy, child: const Text('Copy address')),
                      if (service.kind == 'unknown')
                        TextButton(onPressed: onOpen, child: const Text('Try opening')),
                    ],
            ),
          ],
        ),
      ),
    );
  }
}

/// The host label of a VPN name: the host's name lowercased, each run of characters other than a to z and
/// 0 to 9 replaced by one dash, with dashes trimmed at the ends (spec/ipc.md, decided point 7).
String _vpnHostLabel(String hostName) => hostName
    .toLowerCase()
    .replaceAll(RegExp(r'[^a-z0-9]+'), '-')
    .replaceAll(RegExp(r'^-+|-+$'), '');

/// Asks for the local port of one service. Save returns the port typed when it is from 1 to 65535.
class _PortDialog extends StatefulWidget {
  const _PortDialog({required this.service});

  final String service;

  @override
  State<_PortDialog> createState() => _PortDialogState();
}

class _PortDialogState extends State<_PortDialog> {
  final _controller = TextEditingController();

  /// The message shown under the field after a Save with no valid port, or null.
  String? _error;

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  void _save() {
    final port = int.tryParse(_controller.text);
    if (port == null || port < 1 || port > 65535) {
      setState(() => _error = 'Enter a port from 1 to 65535.');
      return;
    }
    Navigator.pop(context, port);
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text('Local port for ${widget.service}'),
      content: TextField(
        controller: _controller,
        autofocus: true,
        keyboardType: TextInputType.number,
        inputFormatters: [
          FilteringTextInputFormatter.digitsOnly,
          LengthLimitingTextInputFormatter(5),
        ],
        decoration: InputDecoration(errorText: _error),
        onChanged: (_) {
          if (_error != null) setState(() => _error = null);
        },
        onSubmitted: (_) => _save(),
      ),
      actions: [
        TextButton(onPressed: () => Navigator.pop(context), child: const Text('Cancel')),
        TextButton(onPressed: _save, child: const Text('Save')),
      ],
    );
  }
}
