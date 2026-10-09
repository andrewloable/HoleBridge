// The diagnostics screen (docs/architecture.md#state-status-and-logs, docs/cli.md#the-app, decisions
// D25): the route tried, the NAT type, whether a relay key is set, the last error code and the versions,
// with Copy diagnostics and Report a problem. Both copy and report are redacted (diagnostics/redact.dart).
import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:url_launcher/url_launcher.dart';

import '../app_controller.dart';
import '../diagnostics/redact.dart';
import '../engine/ipc_codec.dart';
import '../keys/normalize.dart';
import '../store/host_store.dart';

/// Shows the diagnostics of the app.
///
/// [controller] gives the last error code and rebuilds the screen when it changes. [store] gives the
/// secrets that redaction removes and whether a relay key is set. [route] is the route tried and [nat]
/// the engine's NAT view from its status reply, or null before one arrives. The three versions are the
/// app's, the engine's and the protocol's.
///
/// Copy diagnostics copies a plain-text report to the clipboard. Report a problem opens a prefilled
/// GitHub issue at https://github.com/andrewloable/HoleBridge/issues/new through [openLink]; the app
/// passes its URL launcher, and tests pass a recorder. Both pass the report through redact. Until the
/// store has been read, and when it cannot be read, both buttons are off, so nothing unredacted leaves.
class DiagnosticsScreen extends StatelessWidget {
  const DiagnosticsScreen({
    required this.controller,
    required this.store,
    required this.route,
    required this.nat,
    required this.appVersion,
    required this.engineVersion,
    required this.protocolVersion,
    this.openLink,
    super.key,
  });

  final AppController controller;
  final HostStore store;
  final String route;
  final NatInfo? nat;
  final String appVersion;
  final String engineVersion;
  final String protocolVersion;
  final Future<void> Function(Uri url)? openLink;

  @override
  Widget build(BuildContext context) => ListenableBuilder(
    listenable: controller,
    builder: (context, _) => FutureBuilder<_Stored>(
      future: _loadStored(),
      builder: (context, snapshot) {
        final stored = snapshot.data;
        final facts = _facts(stored?.relaySet);
        final reportText = _reportText(facts);
        return Scaffold(
          appBar: AppBar(title: const Text('Diagnostics')),
          body: SafeArea(
            child: SingleChildScrollView(
              padding: const EdgeInsets.all(16),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Wrap(
                    spacing: 8,
                    runSpacing: 8,
                    children: [
                      _CopyButton(
                        onPressed: stored == null ? null : () => _copy(reportText, stored.secrets),
                      ),
                      OutlinedButton(
                        onPressed: stored == null
                            ? null
                            : () => _openReport(reportText, stored.secrets),
                        child: const Text('Report a problem'),
                      ),
                    ],
                  ),
                  if (snapshot.hasError) ...[
                    const SizedBox(height: 12),
                    const Text(
                      'The stored settings could not be read, so the diagnostics cannot be copied '
                      'or reported.',
                    ),
                  ],
                  const SizedBox(height: 16),
                  for (final (label, value) in facts) _FactRow(label, value),
                  const SizedBox(height: 16),
                  Text(
                    'Copy and report leave out keys, addresses and service names.',
                    style: Theme.of(context).textTheme.bodySmall,
                  ),
                ],
              ),
            ),
          ),
        );
      },
    ),
  );

  /// The facts the screen shows and the report prints, in order. [relaySet] is null until the store has
  /// been read.
  List<(String, String)> _facts(bool? relaySet) => [
    ('Route', route.isEmpty ? 'none' : route),
    ('NAT type', _natType()),
    ('Relay key set', relaySet == null ? 'unknown' : (relaySet ? 'yes' : 'no')),
    ('Last error code', controller.lastErrorCode ?? 'none'),
    ('App version', appVersion),
    ('Engine version', engineVersion),
    ('Protocol version', protocolVersion),
  ];

  String _natType() {
    final nat = this.nat;
    if (nat == null) return 'not known yet';
    final type = nat.randomized ? 'randomized' : 'not randomized';
    return nat.firewalled ? '$type, UDP ports not open yet' : type;
  }

  /// The plain-text report: one line per fact. Redaction also takes any nine-letter word made of Crockford
  /// letters for a key (such as "addresses"), so the fixed wording here must not use one.
  String _reportText(List<(String, String)> facts) => [
    'HoleBridge app diagnostics',
    for (final (label, value) in facts) '$label: $value',
  ].join('\n');

  /// Reads what redaction removes from the store: the host keys (plain and dashed), the application keys
  /// (hex and base64), the relay key (plain and dashed), and the service names and addresses of each host.
  /// The NAT public address is added from [nat].
  Future<_Stored> _loadStored() async {
    final secrets = <String>{};
    for (final host in await store.hosts()) {
      secrets
        ..add(host.key)
        ..add(formatKey(host.key))
        ..addAll(await store.servicesCache(host.id))
        ..addAll(await store.lanAddresses(host.id))
        ..addAll(await store.vpnAddresses(host.id));
    }
    for (final key in await store.appKeys()) {
      secrets
        ..add(_hex(key))
        ..add(base64Encode(key));
    }
    final relay = await store.relayKey();
    if (relay != null) {
      secrets
        ..add(relay)
        ..add(formatKey(relay));
    }
    final publicAddress = nat?.host;
    if (publicAddress != null) secrets.add(publicAddress);
    return _Stored(relaySet: relay != null, secrets: secrets);
  }

  Future<void> _copy(String reportText, Set<String> secrets) async {
    await Clipboard.setData(ClipboardData(text: redact(reportText, secrets)));
  }

  /// Opens the issue form with the title and the body redacted. Uri.https encodes each query value, so
  /// the redacted text reaches the form intact, with its + signs and line breaks.
  Future<void> _openReport(String reportText, Set<String> secrets) async {
    final url = Uri.https('github.com', '/andrewloable/HoleBridge/issues/new', {
      'title': redact('HoleBridge problem report (app $appVersion)', secrets),
      'body': redact(reportText, secrets),
    });
    await (openLink ?? _launch)(url);
  }
}

/// The Copy diagnostics button, the screen's first focus. It is off until the store has been read, and a
/// disabled button cannot take focus, so a plain autofocus (tried at the first build) would miss it. This
/// asks for the focus once the button turns on.
class _CopyButton extends StatefulWidget {
  const _CopyButton({required this.onPressed});

  final VoidCallback? onPressed;

  @override
  State<_CopyButton> createState() => _CopyButtonState();
}

class _CopyButtonState extends State<_CopyButton> {
  final _focus = FocusNode(debugLabel: 'copy-diagnostics');

  /// Whether the focus has been asked for, so a later rebuild does not move it back.
  bool _asked = false;

  @override
  void dispose() {
    _focus.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    if (widget.onPressed != null && !_asked) {
      _asked = true;
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted) _focus.requestFocus();
      });
    }
    return FilledButton(
      focusNode: _focus,
      onPressed: widget.onPressed,
      child: const Text('Copy diagnostics'),
    );
  }
}

/// What the store holds that redaction needs, and whether a relay key is set.
class _Stored {
  const _Stored({required this.relaySet, required this.secrets});

  final bool relaySet;
  final Set<String> secrets;
}

/// One fact on the screen: its label and its value.
class _FactRow extends StatelessWidget {
  const _FactRow(this.label, this.value);

  final String label;
  final String value;

  @override
  Widget build(BuildContext context) => Padding(
    padding: const EdgeInsets.only(bottom: 8),
    child: Row(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        SizedBox(width: 150, child: Text(label, style: Theme.of(context).textTheme.labelLarge)),
        Expanded(child: Text(value)),
      ],
    ),
  );
}

/// The lowercase hex of [bytes], as the app key is typed in a key link.
String _hex(Uint8List bytes) => [for (final b in bytes) b.toRadixString(16).padLeft(2, '0')].join();

Future<void> _launch(Uri url) async {
  await launchUrl(url);
}
