// The add-host flow (docs/cli.md#the-app, docs/cli.md#hosting, docs/architecture.md#adding-a-host-to-a-tv-handoff).
import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../app_controller.dart';
import '../engine/ipc_codec.dart' show IpcDesync;
import '../errors.g.dart';
import '../links/links.dart';
import '../store/host_store.dart';
import 'error_view.dart';
import 'key_entry_field.dart';

/// Builds the camera view of Scan QR. [onCode] is called with the text of each code the camera reads.
/// The app passes the mobile_scanner view; tests pass a fake.
typedef ScannerBuilder = Widget Function(BuildContext context, void Function(String code) onCode);

/// Adds a host: Scan QR, Paste link, Type key, and on Android TV Add from phone.
///
/// A scanned or pasted key link saves the host with its application key and connects it, then shows
/// the screen [hostScreenBuilder] builds for the new host. A typed key goes to
/// AppController.addTypedKey. With no application key held, the screen explains that the first host
/// must come from a QR code, a link or, on a TV, a phone.
///
/// [scannerBuilder] is the camera view. Null when the device has no camera, which hides Scan QR. [isTv]
/// hides Scan QR and shows Add from phone; the app detects a TV, and tests pass it. [onAddFromPhone]
/// runs when Add from phone is pressed. [initialLink] is the link the app was opened with (/k), shown
/// pre-filled. It is not used yet: HoleBridge-hb5.10.8 wires it.
///
/// [onOpenSettings] runs when the Settings button in the app bar is pressed. Null hides the button.
class AddHostScreen extends StatefulWidget {
  const AddHostScreen({
    super.key,
    required this.controller,
    required this.store,
    required this.hostScreenBuilder,
    this.scannerBuilder,
    this.isTv = false,
    this.onAddFromPhone,
    this.initialLink,
    this.onOpenSettings,
  });

  final AppController controller;
  final HostStore store;
  final Widget Function(BuildContext context, String hostId) hostScreenBuilder;
  final ScannerBuilder? scannerBuilder;
  final bool isTv;
  final VoidCallback? onAddFromPhone;
  final String? initialLink;
  final VoidCallback? onOpenSettings;

  @override
  State<AddHostScreen> createState() => _AddHostScreenState();
}

const _noAppKeyNotice =
    'This app does not hold the application key yet, so a typed key cannot be tried. '
    'The first host must come from a QR code, a link or, on a TV, a phone.';
const _engineNotice = 'The app engine did not answer. Try again.';
const _notAddedNotice = 'The host was not added. Try again.';

final _catalogCode = RegExp(r'^(HB-[A-Z0-9-]+):');

/// The key link in [text], or else the code to show for why it is not one. The code is the catalog
/// code that starts the parse error's message, as HB-APPKEY-INVALID does for a bad application key.
/// Any other fault, a handoff link included, shows HB-KEY-INVALID. The code is empty when there is a
/// link. Neither the message nor the link is ever shown.
({KeyLink? link, String code}) _keyLinkIn(String text) {
  try {
    final link = parseAppLink(text);
    if (link is KeyLink) return (link: link, code: '');
    return (link: null, code: 'HB-KEY-INVALID');
  } on FormatException catch (error) {
    final code = _catalogCode.firstMatch(error.message)?.group(1);
    final known = code != null && errorCatalog.containsKey(code);
    return (link: null, code: known ? code : 'HB-KEY-INVALID');
  }
}

/// An engine failure of an add: the engine is not running, did not answer in time, or sent a frame
/// that does not decode. The screen shows it as a notice.
class _EngineFailed implements Exception {}

/// Runs one engine call of an add. A StateError, TimeoutException or IpcDesync becomes _EngineFailed.
/// Any other error is a bug and goes on up unchanged.
Future<T> _engine<T>(Future<T> Function() call) async {
  try {
    return await call();
  } on StateError {
    throw _EngineFailed();
  } on TimeoutException {
    throw _EngineFailed();
  } on IpcDesync {
    throw _EngineFailed();
  }
}

class _AddHostScreenState extends State<AddHostScreen> {
  /// The typed key as the field holds it.
  String _typed = '';

  /// The HB- code of the error shown, or null, and its detail. The caller never puts a key or a link
  /// in the detail.
  String? _errorCode;
  String _errorDetail = '';

  /// A plain notice shown in place of an error, or null.
  String? _notice;

  /// Whether the camera view is open.
  bool _scanning = false;

  /// Set once a key link has been read and is being added. The camera keeps reading the same code, so
  /// its repeats are ignored from then on.
  bool _scanLocked = false;

  /// The last code the camera read, so an invalid code that keeps being read is handled once.
  String? _lastScanned;

  /// True while an add runs. A second press does nothing meanwhile.
  bool _busy = false;

  /// Runs one add. An engine failure (see _engine) is shown as a notice. Any other error propagates,
  /// and the screen is usable again all the same.
  Future<void> _run(Future<void> Function() add) async {
    if (_busy) return;
    setState(() {
      _busy = true;
      _errorCode = null;
      _notice = null;
    });
    try {
      await add();
    } on _EngineFailed {
      _showNotice(_engineNotice);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _pasteLink() => _run(() async {
    final text = (await Clipboard.getData(Clipboard.kTextPlain))?.text ?? '';
    final read = _keyLinkIn(text);
    final link = read.link;
    if (link == null) {
      _showError(read.code);
      return;
    }
    await _addKey(link);
  });

  void _startScan() {
    setState(() {
      _scanning = true;
      _scanLocked = false;
      _lastScanned = null;
      _errorCode = null;
      _notice = null;
    });
  }

  void _stopScan() {
    setState(() => _scanning = false);
  }

  /// The camera read [code]. A key link closes the camera and is added. Anything else shows an error
  /// and the camera keeps reading.
  void _onScanned(String code) {
    if (_scanLocked || code == _lastScanned || !mounted) return;
    _lastScanned = code;
    final read = _keyLinkIn(code);
    final link = read.link;
    if (link == null) {
      _showError(read.code);
      return;
    }
    _scanLocked = true;
    setState(() => _scanning = false);
    unawaited(_run(() => _addKey(link)));
  }

  Future<void> _addTyped() => _run(() async {
    final id = await _engine(() => widget.controller.addTypedKey(_typed));
    if (id != null) {
      _open(id);
      return;
    }
    if ((await widget.store.appKeys()).isEmpty) {
      _showNotice(_noAppKeyNotice);
      return;
    }
    // A null id with a held application key means a failed try, which sets the controller's error.
    final code = widget.controller.lastErrorCode;
    if (code != null) {
      _showError(code);
    } else {
      _showNotice(_notAddedNotice);
    }
  });

  /// Saves the host of [link], or finds it when its key is saved already, connects it, and opens its
  /// screen.
  Future<void> _addKey(KeyLink link) async {
    final id = await _hostIdFor(link);
    await _engine(() => widget.controller.connect(id));
    _open(id);
  }

  Future<String> _hostIdFor(KeyLink link) async {
    for (final host in await widget.store.hosts()) {
      if (host.key == link.key) return host.id;
    }
    final added = await widget.store.addHost(
      name: await _unusedName(),
      key: link.key,
      appKey: link.appKey,
    );
    return added.id;
  }

  /// The first of "Host", "Host 2", "Host 3", ... that no stored host has.
  Future<String> _unusedName() async {
    final taken = {for (final host in await widget.store.hosts()) host.name};
    var name = 'Host';
    for (var n = 2; taken.contains(name); n++) {
      name = 'Host $n';
    }
    return name;
  }

  void _open(String hostId) {
    if (!mounted) return;
    Navigator.of(context).push<void>(
      MaterialPageRoute<void>(
        builder: (routeContext) => widget.hostScreenBuilder(routeContext, hostId),
      ),
    );
  }

  void _showError(String code, [String detail = '']) {
    if (!mounted) return;
    setState(() {
      _errorCode = code;
      _errorDetail = detail;
      _notice = null;
    });
  }

  void _showNotice(String text) {
    if (!mounted) return;
    setState(() {
      _notice = text;
      _errorCode = null;
    });
  }

  @override
  Widget build(BuildContext context) {
    // Scan QR is hidden on a TV, and where the device has no camera.
    final scanner = widget.isTv ? null : widget.scannerBuilder;
    return Scaffold(
      appBar: AppBar(
        title: const Text('New host'),
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
          const Text(
            'Add a host by scanning its code, opening its link, or typing its 9-symbol key.',
          ),
          const SizedBox(height: 16),
          Wrap(
            spacing: 8,
            runSpacing: 8,
            children: [
              if (scanner != null && !_scanning)
                FilledButton(onPressed: _busy ? null : _startScan, child: const Text('Scan QR')),
              OutlinedButton(onPressed: _busy ? null : _pasteLink, child: const Text('Paste link')),
              if (widget.isTv)
                // The first focus of a TV lands here: it is the screen's primary action.
                FilledButton(
                  autofocus: true,
                  onPressed: _busy ? null : widget.onAddFromPhone,
                  child: const Text('Add from phone'),
                ),
            ],
          ),
          if (scanner != null && _scanning) ...[
            const SizedBox(height: 16),
            SizedBox(height: 280, child: scanner(context, _onScanned)),
            TextButton(onPressed: _stopScan, child: const Text('Cancel')),
          ],
          const SizedBox(height: 24),
          const Text('Type key'),
          const SizedBox(height: 8),
          KeyEntryField(
            onChanged: (text) {
              _typed = text;
            },
            onSubmitted: (_) => _addTyped(),
          ),
          const SizedBox(height: 12),
          FilledButton(onPressed: _busy ? null : _addTyped, child: const Text('Add host')),
          if (_busy)
            const Padding(padding: EdgeInsets.only(top: 12), child: LinearProgressIndicator()),
          if (_errorCode != null)
            Padding(
              padding: const EdgeInsets.only(top: 16),
              child: ErrorView(_errorCode!, _errorDetail),
            ),
          if (_notice != null)
            Padding(padding: const EdgeInsets.only(top: 16), child: Text(_notice!)),
        ],
      ),
    );
  }
}
