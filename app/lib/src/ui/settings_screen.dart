// The settings screen (docs/cli.md#the-app, Settings): the relay key, VPN mode (Android only), Share with my
// network for each host, and the Diagnostics row. Every engine request goes through AppController
// (setRelayKey, setShared), so the controller stays the one owner of them.
import 'dart:async';

import 'package:flutter/foundation.dart' show defaultTargetPlatform;
import 'package:flutter/material.dart';
import 'package:flutter/services.dart' show PlatformException;

import '../app_controller.dart';
import '../engine/ipc_codec.dart' show IpcDesync;
import '../keys/normalize.dart' show KeyFormatException;
import '../store/host_store.dart';
import '../vpn/vpn_mode_control.dart';
import 'error_view.dart';

// The controls carry these keys. The settings tests find each control by its key
// (test/ui/settings_screen_test.dart), so the values must stay the same.
const _relayKeyField = Key('relay-key-field');
const _relaySave = Key('relay-save');
const _vpnModeSwitch = Key('vpn-mode-switch');
const _diagnosticsRow = Key('diagnostics-row');
const _shareWarningConfirm = Key('share-warning-confirm');
const _shareWarningCancel = Key('share-warning-cancel');
Key _shareSwitch(String hostId) => Key('share-$hostId');

const _engineNotice = 'The app engine did not answer. Try again.';
const _vpnFailedNotice = 'VPN mode could not be changed. Try again.';
const _relayInvalid = 'Not a valid key. A key is 9 symbols from 0 to 9 and A to Z, except U.';

/// Whether [error] is an engine failure: the engine is not running (StateError), did not answer in time
/// (TimeoutException) or sent a frame that does not decode (IpcDesync). The screen shows these as a notice.
bool _engineFailed(Object error) =>
    error is StateError || error is TimeoutException || error is IpcDesync;

/// The app's settings (docs/cli.md#the-app, Settings).
///
/// Relay: a key typed as 9 symbols is saved with AppController.setRelayKey. The stored key is never shown:
/// the field starts empty, and the screen says only whether a key is set. Saving an empty field removes the
/// key. Share with my network: per host, off by default. Turning it on asks first, and a cancelled warning
/// changes nothing. The change goes through AppController.setShared, and the switch is off while the call
/// runs. VPN mode: the switch is shown on Android only and goes through [vpn]. Diagnostics opens through
/// [onOpenDiagnostics].
class SettingsScreen extends StatefulWidget {
  const SettingsScreen({
    required this.controller,
    required this.store,
    required this.vpn,
    required this.onOpenDiagnostics,
    super.key,
  });

  /// Carries every engine request the screen causes.
  final AppController controller;

  /// Gives the hosts, each host's Share with my network setting, and whether a relay key is stored.
  final HostStore store;

  /// VPN mode, whose switch is shown on Android only.
  final VpnModeControl vpn;

  /// Opens the diagnostics screen.
  final VoidCallback onOpenDiagnostics;

  @override
  State<SettingsScreen> createState() => _SettingsScreenState();
}

class _SettingsScreenState extends State<SettingsScreen> {
  final _relayField = TextEditingController();

  /// Whether a relay key is stored. The key itself is kept nowhere on this screen.
  bool _relaySet = false;

  /// True while a relay save runs. The Save button is off meanwhile.
  bool _relayBusy = false;

  /// The error of the typed key, shown in the field, or null.
  String? _relayError;

  /// The engine notice of the last relay save, or null.
  String? _relayNotice;

  /// The HB- code the relay reply of the last relay save carried when it was refused, or null.
  String? _relayCode;

  /// False until the store has been read. The screen shows a spinner until then.
  bool _loaded = false;

  List<Host> _hosts = const [];

  /// The Share with my network setting each switch shows, by host id.
  final Map<String, bool> _shared = {};

  /// The hosts whose Share call runs. Their switches are off meanwhile.
  final Set<String> _sharing = {};

  /// The engine notice of the last Share call, or null.
  String? _shareNotice;

  /// True while a VPN mode change runs. The switch is off meanwhile.
  bool _vpnBusy = false;

  /// The engine notice of the last VPN mode change, or null.
  String? _vpnNotice;

  @override
  void initState() {
    super.initState();
    unawaited(_load());
  }

  @override
  void dispose() {
    _relayField.dispose();
    super.dispose();
  }

  /// Reads the hosts, each host's Share with my network setting, and whether a relay key is stored.
  Future<void> _load() async {
    final hosts = await widget.store.hosts();
    final shared = <String, bool>{};
    for (final host in hosts) {
      shared[host.id] = await widget.store.shared(host.id);
    }
    final relaySet = (await widget.store.relayKey()) != null;
    if (!mounted) return;
    setState(() {
      _hosts = hosts;
      _shared.addAll(shared);
      _relaySet = relaySet;
      _loaded = true;
    });
  }

  /// Saves the typed relay key, or removes the stored key when the field is empty. A key that does not
  /// normalize shows its error in the field, keeps the text for correction, and sends nothing.
  Future<void> _saveRelay() async {
    if (_relayBusy) return;
    final typed = _relayField.text;
    final clearing = typed.trim().isEmpty;
    setState(() {
      _relayBusy = true;
      _relayError = null;
      _relayNotice = null;
      _relayCode = null;
    });
    var stored = false;
    try {
      // The code is the relay reply's, so an error left by an earlier call does not show here.
      final code = await widget.controller.setRelayKey(typed);
      stored = true;
      _relayCode = code;
    } on KeyFormatException {
      // setRelayKey throws before it stores anything.
      _relayError = _relayInvalid;
    } catch (error) {
      if (!_engineFailed(error)) rethrow;
      // setRelayKey stores the key before it asks the engine, so the key is stored even though the engine
      // did not answer.
      stored = true;
      _relayNotice = _engineNotice;
    } finally {
      if (mounted) {
        setState(() {
          _relayBusy = false;
          if (stored) {
            _relaySet = !clearing;
            _relayField.clear();
          }
        });
      }
    }
  }

  /// Turns Share with my network on or off for [host]. Turning it on asks first. The switch is off while the
  /// call runs, and afterwards it shows the setting the store holds, which is the one the next connect uses,
  /// so a failed call still shows what applies.
  Future<void> _changeShare(Host host, bool on) async {
    if (_sharing.contains(host.id)) return;
    if (on) {
      final confirmed = await showDialog<bool>(
        context: context,
        builder: (context) => AlertDialog(
          title: const Text('Share with my network?'),
          content: const Text(
            "Other devices on this network will be able to reach this host's services through this device. "
            'Turn this on only if you trust every device on the network.',
          ),
          actions: [
            TextButton(
              key: _shareWarningCancel,
              onPressed: () => Navigator.of(context).pop(false),
              child: const Text('Cancel'),
            ),
            FilledButton(
              key: _shareWarningConfirm,
              onPressed: () => Navigator.of(context).pop(true),
              child: const Text('Turn on'),
            ),
          ],
        ),
      );
      if (confirmed != true || !mounted) return;
    }
    setState(() {
      _sharing.add(host.id);
      _shared[host.id] = on;
      _shareNotice = null;
    });
    var failed = false;
    try {
      await widget.controller.setShared(host.id, on);
    } catch (error) {
      if (!_engineFailed(error)) rethrow;
      failed = true;
    } finally {
      // Read back from the store: the setting is saved before the host reconnects, so it holds even when
      // the reconnect failed.
      final stored = await widget.store.shared(host.id);
      if (mounted) {
        setState(() {
          _sharing.remove(host.id);
          _shared[host.id] = stored;
          _shareNotice = failed ? _engineNotice : null;
        });
      }
    }
  }

  /// Turns VPN mode on or off. A refused consent leaves it off, as VpnModeControl.enable reports it. A
  /// PlatformException from the channel, or an engine failure, shows a notice and leaves the switch as it
  /// was. Any other error is a bug and propagates.
  Future<void> _changeVpn(bool on) async {
    if (_vpnBusy) return;
    setState(() {
      _vpnBusy = true;
      _vpnNotice = null;
    });
    String? notice;
    try {
      if (on) {
        await widget.vpn.enable();
      } else {
        await widget.vpn.disable();
      }
    } catch (error) {
      if (error is PlatformException) {
        // The channel's code and message stay out of the notice.
        notice = _vpnFailedNotice;
      } else if (_engineFailed(error)) {
        notice = _engineNotice;
      } else {
        rethrow;
      }
    } finally {
      if (mounted) {
        setState(() {
          _vpnBusy = false;
          _vpnNotice = notice;
        });
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final text = Theme.of(context).textTheme;
    return Scaffold(
      appBar: AppBar(title: const Text('Settings')),
      body: !_loaded
          ? const Center(child: CircularProgressIndicator())
          : ListView(
              padding: const EdgeInsets.all(16),
              children: [
                Text('Relay', style: text.titleMedium),
                const SizedBox(height: 4),
                Text(_relaySet ? 'A relay key is set.' : 'No relay key is set.'),
                const SizedBox(height: 8),
                TextField(
                  key: _relayKeyField,
                  controller: _relayField,
                  // The relay key is a secret: the text is hidden as it is typed, and the stored key is never shown.
                  obscureText: true,
                  enableSuggestions: false,
                  autocorrect: false,
                  textInputAction: TextInputAction.done,
                  // Done on a field with no text changes nothing. Only the Save button removes the stored key.
                  onSubmitted: (text) {
                    if (text.trim().isNotEmpty) unawaited(_saveRelay());
                  },
                  decoration: InputDecoration(
                    labelText: 'Relay key',
                    hintText: 'XXX-XXX-XXX',
                    helperText:
                        'Needed only when both ends are on networks that cannot hole-punch. '
                        'Save an empty field to remove the key.',
                    errorText: _relayError,
                    border: const OutlineInputBorder(),
                  ),
                ),
                const SizedBox(height: 12),
                FilledButton(
                  key: _relaySave,
                  onPressed: _relayBusy ? null : _saveRelay,
                  child: const Text('Save'),
                ),
                if (_relayBusy)
                  const Padding(
                    padding: EdgeInsets.only(top: 12),
                    child: LinearProgressIndicator(),
                  ),
                if (_relayNotice != null)
                  Padding(
                    padding: const EdgeInsets.only(top: 12),
                    child: Text(_relayNotice!),
                  ),
                if (_relayCode != null)
                  Padding(
                    padding: const EdgeInsets.only(top: 12),
                    child: ErrorView(_relayCode!, ''),
                  ),
                const SizedBox(height: 24),
                if (defaultTargetPlatform == TargetPlatform.android) ...[
                  Text('VPN mode', style: text.titleMedium),
                  SwitchListTile(
                    key: _vpnModeSwitch,
                    contentPadding: EdgeInsets.zero,
                    title: const Text('Reach services by name from every app'),
                    subtitle: const Text(
                      'Every app on this device reaches the services by name, even with HoleBridge closed. '
                      'Nothing else goes through it.',
                    ),
                    value: widget.vpn.enabled,
                    onChanged: _vpnBusy ? null : _changeVpn,
                  ),
                  if (_vpnNotice != null) Text(_vpnNotice!),
                  const SizedBox(height: 24),
                ],
                Text('Share with my network', style: text.titleMedium),
                const SizedBox(height: 4),
                const Text(
                  'Without VPN mode, per host, off by default. Other devices on this network can then use '
                  "the host's services.",
                ),
                if (_hosts.isEmpty)
                  const Padding(
                    padding: EdgeInsets.only(top: 8),
                    child: Text('Add a host to share it.'),
                  ),
                for (final host in _hosts)
                  SwitchListTile(
                    key: _shareSwitch(host.id),
                    contentPadding: EdgeInsets.zero,
                    title: Text(host.name),
                    value: _shared[host.id] ?? false,
                    onChanged: _sharing.contains(host.id) ? null : (on) => _changeShare(host, on),
                  ),
                if (_shareNotice != null)
                  Padding(
                    padding: const EdgeInsets.only(top: 8),
                    child: Text(_shareNotice!),
                  ),
                const SizedBox(height: 24),
                ListTile(
                  key: _diagnosticsRow,
                  contentPadding: EdgeInsets.zero,
                  title: const Text('Diagnostics'),
                  subtitle: const Text(
                    'The route, NAT type, relay status, last error code and versions.',
                  ),
                  trailing: const Icon(Icons.chevron_right),
                  onTap: widget.onOpenDiagnostics,
                ),
              ],
            ),
    );
  }
}
