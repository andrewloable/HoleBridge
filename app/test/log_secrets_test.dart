// Proves that no line the app prints carries a secret or the payload marker (docs/security.md, rules for
// implementers, rule 1). The app flows (start with a relay key, connect, reconnect, a typed add, a failed
// add, a worklet crash and a desync) run over the fake engine, and so do the diagnostics copy and report, the
// relay key and Share with my network, and the settings, host and add-host screens.
// The capture covers debugPrint, print, FlutterError reports, and dart:io stdout and stderr. A dart:developer
// log cannot be captured from a test, so the last test scans the app's lib/ for it and for the logging
// packages instead.
import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/app_controller.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';
import 'package:holebridge/src/keys/normalize.dart';
import 'package:holebridge/src/links/links.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';
import 'package:holebridge/src/ui/add_host_screen.dart';
import 'package:holebridge/src/ui/diagnostics_screen.dart';
import 'package:holebridge/src/ui/host_screen.dart';
import 'package:holebridge/src/ui/settings_screen.dart';
import 'package:holebridge/src/vpn/vpn_mode_control.dart';

import 'helpers/fake_engine_host.dart';

/// An in-memory SecureBackend, as the one in test/app_controller_test.dart is.
class _MemoryBackend implements SecureBackend {
  final Map<String, String> _entries = {};

  @override
  Future<String?> read(String k) async => _entries[k];

  @override
  Future<void> write(String k, String v) async {
    _entries[k] = v;
  }

  @override
  Future<void> delete(String k) async {
    _entries.remove(k);
  }
}

// Test values only (docs/security.md). The 9-symbol keys use the Crockford alphabet and look real, so a
// leak would look like a real leak. The application key is 32 bytes.
final _appKey = Uint8List.fromList(List<int>.generate(32, (i) => (i * 37 + 11) & 0xff));
const _hostName = 'Home';
const _hostKey = '7KQM4X9TR';
const _relayKey = 'ZXVTSRQPN';
// The relay key typed by hand, in lower case with dashes. It is the relay key in one more form.
const _relayTyped = 'zxv-tsr-qpn';
// A key typed by hand, in lower case with dashes. It normalizes to _typedKeyNormalized.
const _typedKey = 'pqr-stv-wxy';
const _typedKeyNormalized = 'PQRSTVWXY';
// A key typed for the failed add. The fake engine never finds its host.
const _unfoundKey = 'HJKMNPQRS';
// Service names and addresses are redacted from diagnostics (docs/security.md), so they stay out of the
// log as well.
const _serviceName = 'photo-library';
const _lanAddress = '192.168.1.20';
const _natAddress = '203.0.113.7';
// A unique marker for the free text the engine sends: error details, reject reasons and a crash detail.
// The app keeps none of that text, so the marker must never reach a line. It stands in for payload bytes
// and forwarded DNS names, which must not reach one either.
const _payloadMarker = 'payload-marker-4f9d2b71c3';

String _hex(Uint8List bytes) => [for (final b in bytes) b.toRadixString(16).padLeft(2, '0')].join();

final _appKeyHex = _hex(_appKey);

// The handoff secret of a handoff link, 32 hex digits. A test value, as the others are.
final _handoffSecretHex = List.filled(16, 'a5').join();

// The links the add-host flows paste. A key link carries the host key and the application key. A handoff
// link carries a one-time public key, a one-time secret and the TV's LAN addresses. Each is a secret, as text
// and by its fragment. The handoff public key is the application key's hex, a test value.
final _validLink = buildKeyLink('https://example.com', formatKey(_hostKey), _appKey);
final _noAppKeyLink = 'https://example.com/k#${formatKey(_hostKey)}';
final _badAppKeyLink = 'https://example.com/k#${formatKey(_hostKey)}.${_appKeyHex.toUpperCase()}';
final _handoffLink = 'https://example.com/h#1.$_appKeyHex.$_handoffSecretHex.49152.$_lanAddress';

/// The forms a 9-symbol key takes in text: normalized, dashed, and dashed with the dashes percent-encoded
/// as a key link carries them. The UTF-8 bytes are how an IPC frame carries the key, and Dart prints a byte
/// list as "[55, 75, ...]" (Uint8List.toString()) or, unbracketed, as "55, 75, ...".
Map<String, String> _keyForms(String what, String normalized) => {
  '$what, normalized': normalized,
  '$what, dashed': formatKey(normalized),
  '$what, dashes percent-encoded': formatKey(normalized).replaceAll('-', '%2D'),
  '$what, UTF-8 bytes as a Dart list': utf8.encode(normalized).join(', '),
  '$what, UTF-8 bytes as a Dart list, Uint8List.toString()': utf8.encode(normalized).toString(),
};

/// The forms the application key takes in text: hex, base64 and base64url, padded and unpadded, and the bytes
/// as Dart prints a byte list, bracketed and unbracketed.
Map<String, String> _appKeyForms(String what, Uint8List bytes) => {
  '$what, hex': _hex(bytes),
  '$what, base64': base64Encode(bytes),
  '$what, base64 without padding': base64Encode(bytes).replaceAll('=', ''),
  '$what, base64url': base64Url.encode(bytes),
  '$what, base64url without padding': base64Url.encode(bytes).replaceAll('=', ''),
  '$what, as a Dart list': bytes.join(', '),
  '$what, as a Dart list, Uint8List.toString()': bytes.toString(),
};

/// Every secret the flows use, in each form, by the name of the secret. The check ignores case, as the
/// redaction does.
final Map<String, String> _secrets = {
  ..._keyForms('the host key', _hostKey),
  ..._keyForms('the relay key', _relayKey),
  'the relay key, as typed': _relayTyped,
  ..._keyForms('the typed key', _typedKeyNormalized),
  'the typed key, as typed': _typedKey,
  ..._keyForms('the key that is not found', _unfoundKey),
  ..._appKeyForms('the application key', _appKey),
  'the service name': _serviceName,
  'the LAN address': _lanAddress,
  'the NAT public address': _natAddress,
};

/// The forms of the links the add-host flows paste, as text and by fragment, and the handoff secret.
Map<String, String> _linkForms(String what, String link) => {
  '$what, as pasted': link,
  '$what, fragment': link.substring(link.indexOf('#') + 1),
};

/// Every link form and the handoff secret, by the name of the secret. The add-host flows check against these
/// as well as [_secrets].
final Map<String, String> _linkSecrets = {
  ..._linkForms('the key link', _validLink),
  ..._linkForms('the key link with no application key', _noAppKeyLink),
  ..._linkForms('the key link with a bad application key', _badAppKeyLink),
  ..._linkForms('the handoff link', _handoffLink),
  'the handoff secret, hex': _handoffSecretHex,
};

/// What the add-host flows must not show or print: every secret and every link form.
final Map<String, String> _addHostSecrets = {..._secrets, ..._linkSecrets};

/// What the host screen must not show or print. It shows its service names and local ports on purpose, so the
/// service name is left out; every other secret stays.
final Map<String, String> _hostScreenSecrets = {
  for (final entry in _secrets.entries)
    if (entry.key != 'the service name') entry.key: entry.value,
};

/// The forms only the log check looks for. The app writes an application key as hex or padded base64, and
/// redaction is built for the forms the store holds, so a route carrying the other forms would not be redacted.
const _logOnlyForms = ['base64url', 'without padding', 'as a Dart list', 'UTF-8 bytes'];

/// Every secret the flows use, joined by spaces, for the route field of the diagnostics. The forms in
/// [_logOnlyForms] are left out, so the route carries only forms that redaction removes.
final _routeWithSecrets = [
  for (final entry in _secrets.entries)
    if (!_logOnlyForms.any((form) => entry.key.contains(form))) entry.value,
].join(' ');

/// The names of the secrets, and of the payload marker, that [line] holds. Empty when it holds none. [secrets]
/// is what the check looks for, [_secrets] when it is not given.
List<String> _secretsIn(String line, [Map<String, String>? secrets]) {
  final lower = line.toLowerCase();
  return [
    for (final entry in (secrets ?? _secrets).entries)
      if (lower.contains(entry.value.toLowerCase())) entry.key,
    if (line.contains(_payloadMarker)) 'the payload marker',
  ];
}

/// Fails for each line that holds a secret or the marker. The failure names what the line holds and never
/// the line, so the failure output does not repeat the secret.
void _expectNoSecretIn(List<String> lines, {required String where, Map<String, String>? secrets}) {
  for (var i = 0; i < lines.length; i++) {
    final found = _secretsIn(lines[i], secrets);
    expect(
      found,
      isEmpty,
      reason: '$where: printed line ${i + 1} of ${lines.length} holds ${found.join(', ')}',
    );
  }
}

/// The log channels the capture cannot see, as text: a dart:developer log, and the logging packages (the app
/// has none). A library that names one of them could log outside the capture.
const _unseenLogChannels = ['dart:developer', 'package:logging/', 'package:logger/'];

/// The unseen log channels that [source] names. Empty when it names none.
List<String> _unseenLogChannelsIn(String source) => [
  for (final channel in _unseenLogChannels)
    if (source.contains(channel)) channel,
];

/// What the capture wrote while a flow ran: debugPrint, print, FlutterError reports, stdout and stderr.
final List<String> _lines = [];

/// The text of the FlutterError sentinel. The capture does not pass it on to the handler that was set before.
const _flutterErrorSentinel = 'sentinel: FlutterError';

/// A stdout or stderr that keeps each text written to it in [_sink] instead of the console. The flows write
/// only these members. Every other member goes to noSuchMethod, which returns null.
class _CapturingStdout implements Stdout {
  _CapturingStdout(this._sink);

  final List<String> _sink;

  @override
  void write(Object? object) => _sink.add('$object');

  @override
  void writeln([Object? object = '']) => _sink.add('$object');

  @override
  void writeAll(Iterable<dynamic> objects, [String separator = '']) =>
      _sink.add(objects.join(separator));

  @override
  void writeCharCode(int charCode) => _sink.add(String.fromCharCode(charCode));

  @override
  void add(List<int> data) => _sink.add(utf8.decode(data, allowMalformed: true));

  @override
  dynamic noSuchMethod(Invocation invocation) => null;
}

/// Runs [flow] with every channel a line can take routed into [_lines]: debugPrint, print (a zone spec),
/// FlutterError reports (FlutterError.onError), and dart:io stdout and stderr (IOOverrides). Each hook is
/// restored before this returns: flutter_test fails a test that leaves one changed.
Future<void> _capturing(Future<void> Function() flow) async {
  _lines.clear();
  final debugPrintBefore = debugPrint;
  final onErrorBefore = FlutterError.onError;
  debugPrint = (String? message, {int? wrapWidth}) {
    if (message != null) _lines.add(message);
  };
  FlutterError.onError = (details) {
    _lines.add(details.toString());
    _lines.add(details.exceptionAsString());
    // Any other report also goes to the handler that was set before, so a widget test still fails on a
    // framework error, as it does without the capture. The sentinel goes to the capture only.
    if (details.exception != _flutterErrorSentinel) onErrorBefore?.call(details);
  };
  try {
    await IOOverrides.runZoned(
      () => runZoned(
        flow,
        zoneSpecification: ZoneSpecification(print: (self, parent, zone, line) => _lines.add(line)),
      ),
      stdout: () => _CapturingStdout(_lines),
      stderr: () => _CapturingStdout(_lines),
    );
  } finally {
    debugPrint = debugPrintBefore;
    FlutterError.onError = onErrorBefore;
  }
}

/// Proves the capture sees every channel, so that an empty capture means that no line was printed. Each
/// sentinel must appear in the capture (a FlutterError report adds more than one line, so this checks by
/// containment), and the capture is cleared after.
void _expectCaptureWorks() {
  debugPrint('sentinel: debugPrint');
  // The app never calls print; the sentinel uses it only to prove the zone capture sees it.
  // ignore: avoid_print
  print('sentinel: print');
  FlutterError.reportError(FlutterErrorDetails(exception: _flutterErrorSentinel));
  stdout.writeln('sentinel: stdout');
  stderr.writeln('sentinel: stderr');
  for (final sentinel in [
    'sentinel: debugPrint',
    'sentinel: print',
    _flutterErrorSentinel,
    'sentinel: stdout',
    'sentinel: stderr',
  ]) {
    expect(
      _lines.any((line) => line.contains(sentinel)),
      isTrue,
      reason: 'the capture sees "$sentinel"',
    );
  }
  _lines.clear();
}

/// A fake engine that answers every request ok.
FakeEngineHost _answeringHost() {
  final host = FakeEngineHost();
  host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));
  return host;
}

/// The text each Text widget shows, as the screen gives it, including the screens beneath a pushed route
/// (offstage). A field's own content is not a Text, so the key the user types into a field is not in this
/// list (it is the user's input).
List<String> _shownTexts(WidgetTester tester) => [
  for (final widget in tester.widgetList<Text>(find.byType(Text, skipOffstage: false)))
    widget.data ?? widget.textSpan?.toPlainText() ?? '',
];

/// The clipboard as a mock: Paste link reads [text], and each Clipboard.setData is kept in [copied].
class _Clipboard {
  String text = '';
  final List<String> copied = [];
}

/// Mocks the platform clipboard until the test ends, and returns the mock.
_Clipboard _mockClipboard(WidgetTester tester) {
  final clipboard = _Clipboard();
  tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (
    call,
  ) async {
    if (call.method == 'Clipboard.getData') return <String, dynamic>{'text': clipboard.text};
    if (call.method == 'Clipboard.setData') {
      clipboard.copied.add((call.arguments as Map)['text'] as String);
    }
    return null;
  });
  addTearDown(
    () => tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(
      SystemChannels.platform,
      null,
    ),
  );
  return clipboard;
}

// The controls of the settings screen carry these keys (lib/src/ui/settings_screen.dart), as its tests find them.
const _relayFieldKey = Key('relay-key-field');
const _relaySaveKey = Key('relay-save');
const _shareWarningConfirmKey = Key('share-warning-confirm');
Key _shareSwitchKey(String hostId) => Key('share-$hostId');

const _hostScreenKey = Key('host-screen-stub');

/// Stands in for the host screen, which the host screen's own tests cover.
Widget _hostScreenStub(BuildContext context, String hostId) =>
    const Center(key: _hostScreenKey, child: Text('Host screen'));

/// Lets frames, animations and the engine's replies run, as the add-host tests do.
Future<void> _settle(WidgetTester tester) async {
  await tester.pump();
  await tester.pump(const Duration(seconds: 1));
}

/// VPN mode for the settings screen, which only asks for its state and its switch.
class _NoVpnMode implements VpnModeControl {
  @override
  bool enabled = false;

  @override
  Future<void> enable() async {
    enabled = true;
  }

  @override
  Future<void> disable() async {
    enabled = false;
  }
}

void main() {
  test('the check finds every form of every secret, so a clean capture means something', () {
    for (final entry in _secrets.entries) {
      expect(_secretsIn('printed: ${entry.value} end'), contains(entry.key));
    }
    expect(_secretsIn('printed: $_payloadMarker'), ['the payload marker']);
    expect(
      _secretsIn('route direct, error HB-LOOKUP-TIMEOUT, versions 1.0.0+1, 3.1.4, 1'),
      isEmpty,
    );
  });

  group('the controller flows', () {
    test('start, connect, reconnect, a typed add, a failed add, a crash and a desync print no secret', () async {
      final store = HostStore(_MemoryBackend());
      final home = await store.addHost(name: _hostName, key: _hostKey, appKey: _appKey);
      await store.saveRelayKey(_relayKey);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);

      await _capturing(() async {
        _expectCaptureWorks();

        // Relay: start() sends the stored relay key with the held application key.
        await controller.start();
        expect(host.requestsOf<RelayRequest>().single.key, _relayKey);

        // Connect: the host key and application key go to the engine. The engine's events name the host.
        await controller.connect(home.id);
        host.deliver(encode(RouteEvent(host: _hostName, route: 'direct')));
        host.deliver(
          encode(LanEvent(host: _hostName, addresses: const [_lanAddress], port: 49152)),
        );
        host.deliver(
          encode(
            ServicesEvent(
              host: _hostName,
              list: const [ServiceEntry(name: _serviceName, kind: 2)],
              ports: const [PortBinding(service: _serviceName, port: 8080)],
            ),
          ),
        );
        await pumpEventQueue();

        // Reconnect: close the host, then connect it again with the store as it is now.
        await controller.reconnect(home.id);

        // Add a host by its typed key. The engine answers the first try.
        expect(await controller.addTypedKey(_typedKey), isNotNull);

        // A failed add. The engine's free text carries every secret and the marker, and the controller must
        // not print any of it.
        host.onRequest = (request) => host.deliver(
          encode(
            IpcReply(
              id: request.id,
              ok: request is! ConnectRequest,
              code: request is ConnectRequest ? 'HB-LOOKUP-TIMEOUT' : '',
              detail:
                  'search gave up: $_payloadMarker $_hostKey $_relayKey $_serviceName $_lanAddress',
            ),
          ),
        );
        expect(await controller.addTypedKey(_unfoundKey), isNull);
        host.deliver(
          encode(
            RejectEvent(
              host: _hostName,
              service: _serviceName,
              code: 'HB-LOOKUP-TIMEOUT',
              reason: '$_payloadMarker $_hostKey $_serviceName',
            ),
          ),
        );
        host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));

        // A worklet crash whose detail names a secret and the marker. The controller restarts the engine.
        host.crash((reason: 'exit', detail: 'worklet exited: $_payloadMarker $_appKeyHex'));
        await pumpEventQueue();
        expect(host.calls, ['start', 'stop', 'start'], reason: 'the crash restarted the engine');

        // The restarted engine has no registrations and no relay, so the next connect sends both again.
        await controller.connect(home.id);
        expect(host.requestsOf<RelayRequest>(), hasLength(2));

        // A desync: a frame from the engine did not decode. Its detail carries the marker.
        host.deliver(encode(ErrorEvent(code: 'HB-IPC-DESYNC', detail: 'frame: $_payloadMarker')));
        await pumpEventQueue();
      });

      // The flows really carried the secrets, so an empty capture means that none was printed.
      final sentKeys = host.requests.whereType<ConnectRequest>().map((r) => r.key).toSet();
      expect(sentKeys, containsAll([_hostKey, _typedKeyNormalized, _unfoundKey]));
      expect(host.requests.whereType<RelayRequest>().map((r) => r.key).toSet(), {_relayKey});
      for (final value in [...sentKeys, _relayKey]) {
        expect(_secretsIn(value), isNotEmpty, reason: 'a sent key is one the check looks for');
      }
      final appKeysSent = host.requests
          .whereType<ConnectRequest>()
          .map((r) => base64Encode(r.appKey))
          .toSet();
      expect(appKeysSent, {base64Encode(_appKey)});
      expect(
        _secretsIn(base64Encode(_appKey)),
        isNotEmpty,
        reason: 'the sent application key is one the check looks for',
      );

      _expectNoSecretIn(_lines, where: 'the controller flows');
    });
  });

  group('the diagnostics', () {
    testWidgets('copy diagnostics and report a problem print no secret', (tester) async {
      tester.view.physicalSize = const Size(1000, 30000);
      tester.view.devicePixelRatio = 1.0;
      addTearDown(tester.view.reset);

      await _capturing(() => _copyAndReport(tester));

      _expectNoSecretIn(_lines, where: 'the diagnostics copy and report');
    });
  });

  group('the relay key and Share with my network', () {
    test('setRelayKey and setShared print no secret, and the engine gets their requests', () async {
      final store = HostStore(_MemoryBackend());
      final home = await store.addHost(name: _hostName, key: _hostKey, appKey: _appKey);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();
      await controller.connect(home.id);
      final mark = host.sent.length;

      await _capturing(() async {
        _expectCaptureWorks();

        // A relay key typed in lower case with dashes is stored, and sent at once with the held application key.
        expect(await controller.setRelayKey(_relayTyped), isNull);
        expect(await store.relayKey(), _relayKey);
        // Cleared: the empty relay request goes out, and nothing is stored.
        expect(await controller.setRelayKey(null), isNull);
        expect(await store.relayKey(), isNull);

        // Share with my network on, then off. Each change closes the host and connects it again with its bind.
        await controller.setShared(home.id, true);
        await controller.setShared(home.id, false);
      });

      final sent = host.requests.skip(mark).toList();
      expect(
        sent,
        hasLength(6),
        reason: 'two relay requests, then a close and a connect for each share change',
      );
      final relay = sent[0] as RelayRequest;
      expect(relay.key, _relayKey);
      expect(relay.appKey, _appKey, reason: 'the relay carries the held application key');
      expect((sent[1] as RelayRequest).key, '', reason: 'clearing the relay sends an empty key');
      expect(sent[2], isA<CloseRequest>());
      final shared = sent[3] as ConnectRequest;
      expect(shared.key, _hostKey);
      expect(shared.appKey, _appKey);
      expect(shared.bind, '0.0.0.0');
      expect(sent[4], isA<CloseRequest>());
      expect((sent[5] as ConnectRequest).bind, '127.0.0.1');

      _expectNoSecretIn(_lines, where: 'setRelayKey and setShared');
    });
  });

  group('the settings screen', () {
    testWidgets(
      'a typed relay key is saved and Share with my network is turned on, with no secret printed or shown',
      (tester) async {
        tester.view.physicalSize = const Size(800, 2400);
        tester.view.devicePixelRatio = 1.0;
        addTearDown(tester.view.reset);

        final store = HostStore(_MemoryBackend());
        final home = await store.addHost(name: _hostName, key: _hostKey, appKey: _appKey);
        // A relay key is stored before the screen opens, so the screen's first read holds a secret to show.
        await store.saveRelayKey(_relayKey);
        final host = _answeringHost();
        addTearDown(host.close);
        final controller = AppController(host, store);
        addTearDown(controller.dispose);
        await controller.start();
        await controller.connect(home.id);
        final mark = host.sent.length;

        await _capturing(() async {
          _expectCaptureWorks();
          await tester.pumpWidget(
            MaterialApp(
              home: SettingsScreen(
                controller: controller,
                store: store,
                vpn: _NoVpnMode(),
                onOpenDiagnostics: () {},
              ),
            ),
          );
          await tester.pumpAndSettle();
          // The screen has read the stored key: it says only that one is set.
          expect(find.text('A relay key is set.'), findsOneWidget);
          _expectNoSecretIn(
            _shownTexts(tester),
            where: 'the settings screen with a relay key stored, shown text',
          );

          // Type a relay key and save it. The field is cleared after the save.
          await tester.enterText(find.byKey(_relayFieldKey), _relayTyped);
          await tester.tap(find.byKey(_relaySaveKey));
          await tester.pumpAndSettle();
          _expectNoSecretIn(
            _shownTexts(tester),
            where: 'the settings screen after the relay save, shown text',
          );

          // Turn Share with my network on for the host: the warning comes first, then the confirm.
          await tester.tap(find.byKey(_shareSwitchKey(home.id)));
          await tester.pumpAndSettle();
          await tester.tap(find.byKey(_shareWarningConfirmKey));
          await tester.pumpAndSettle();
        });

        // The flows carried the secrets: the relay key reached the engine, and the host reconnected with bind 0.0.0.0.
        expect(await store.relayKey(), _relayKey);
        final sent = host.requests.skip(mark).toList();
        expect(sent.whereType<RelayRequest>().single.key, _relayKey);
        expect(sent.whereType<ConnectRequest>().single.bind, '0.0.0.0');

        // The relay key field is the user's input: its content is an EditableText, which the shown text does not
        // include. Everything else on the screen, and everything printed, holds no secret.
        _expectNoSecretIn(_lines, where: 'the settings screen, printed');
        _expectNoSecretIn(_shownTexts(tester), where: 'the settings screen, shown text');
      },
    );
  });

  group('the host screen', () {
    testWidgets('its tiles, the port dialog and Copy address print and show no secret', (
      tester,
    ) async {
      tester.view.physicalSize = const Size(800, 2000);
      tester.view.devicePixelRatio = 1.0;
      addTearDown(tester.view.reset);
      final clipboard = _mockClipboard(tester);

      final store = HostStore(_MemoryBackend());
      final home = await store.addHost(name: _hostName, key: _hostKey, appKey: _appKey);
      await store.saveRelayKey(_relayKey);
      final host = FakeEngineHost();
      addTearDown(host.close);
      // A reachable engine: a connect is ok, with the route, the service and the port it was asked for.
      host.onRequest = (request) {
        if (request is ConnectRequest) {
          final asked = {for (final port in request.ports) port.service: port.port};
          host.deliver(
            encode(
              IpcReply(
                id: request.id,
                ok: true,
                route: 'lan',
                services: const [ServiceEntry(name: _serviceName, kind: 3)],
                ports: [PortBinding(service: _serviceName, port: asked[_serviceName] ?? 8080)],
              ),
            ),
          );
        } else {
          host.deliver(encode(IpcReply(id: request.id, ok: true)));
        }
      };
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();

      await _capturing(() async {
        _expectCaptureWorks();
        await tester.pumpWidget(
          MaterialApp(
            home: HostScreen(controller: controller, store: store, hostId: home.id),
          ),
        );
        await tester.pumpAndSettle();

        // The engine reports the route, the LAN address, the service with its port, and a reject and an error
        // whose free text holds every secret and the marker. The screen keeps none of that text.
        host.deliver(encode(RouteEvent(host: _hostName, route: 'lan')));
        host.deliver(
          encode(LanEvent(host: _hostName, addresses: const [_lanAddress], port: 49152)),
        );
        host.deliver(
          encode(
            ServicesEvent(
              host: _hostName,
              list: const [ServiceEntry(name: _serviceName, kind: 3)],
              ports: const [PortBinding(service: _serviceName, port: 8080)],
            ),
          ),
        );
        host.deliver(
          encode(
            RejectEvent(
              host: _hostName,
              service: _serviceName,
              code: 'HB-LOOKUP-TIMEOUT',
              reason: '$_payloadMarker $_hostKey $_relayKey',
            ),
          ),
        );
        host.deliver(
          encode(ErrorEvent(code: 'HB-LOOKUP-TIMEOUT', detail: '$_payloadMarker $_appKeyHex')),
        );
        await tester.pumpAndSettle();
        _expectNoSecretIn(
          _shownTexts(tester),
          where: 'the host screen after its events, shown text',
          secrets: _hostScreenSecrets,
        );

        // Tap the tile: the port dialog opens. Save a port, and the host reconnects with it.
        await tester.tap(find.text(_serviceName));
        await tester.pumpAndSettle();
        await tester.enterText(find.byType(TextField), '8081');
        await tester.tap(find.text('Save'));
        await tester.pumpAndSettle();

        // Copy address copies the local address of the service.
        await tester.tap(find.text('Copy address'));
        await tester.pumpAndSettle();
      });

      // The flows carried the secrets: the relay key went out, and the host connected with its key and application key.
      expect(host.requestsOf<RelayRequest>().first.key, _relayKey);
      final connects = host.requestsOf<ConnectRequest>().toList();
      expect(connects.first.key, _hostKey);
      expect(connects.first.appKey, _appKey);
      expect(connects.last.ports.single.port, 8081, reason: 'the reconnect carries the saved port');
      expect(clipboard.copied, ['127.0.0.1:8081'], reason: 'Copy address wrote the local address');

      // The service name and the local port are shown on purpose, so they are left out of this check.
      _expectNoSecretIn(_lines, where: 'the host screen, printed', secrets: _hostScreenSecrets);
      _expectNoSecretIn(
        clipboard.copied,
        where: 'the host screen, copied',
        secrets: _hostScreenSecrets,
      );
      _expectNoSecretIn(
        _shownTexts(tester),
        where: 'the host screen, shown text',
        secrets: _hostScreenSecrets,
      );
    });
  });

  group('the add-host screen', () {
    test('the check finds every link form and the handoff secret', () {
      for (final entry in _linkSecrets.entries) {
        expect(_secretsIn('printed: ${entry.value} end', _addHostSecrets), contains(entry.key));
      }
    });

    testWidgets('a valid key link adds the host and prints and shows no secret', (tester) async {
      final clipboard = _mockClipboard(tester);
      clipboard.text = _validLink;
      final store = HostStore(_MemoryBackend());
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();

      await _capturing(() async {
        _expectCaptureWorks();
        await tester.pumpWidget(
          MaterialApp(
            home: AddHostScreen(
              controller: controller,
              store: store,
              hostScreenBuilder: _hostScreenStub,
            ),
          ),
        );
        await tester.pumpAndSettle();
        await tester.tap(find.text('Paste link'));
        await _settle(tester);
      });

      // The link added the host and opened its screen, so the check covers the add.
      expect(find.byKey(_hostScreenKey), findsOneWidget);
      expect(host.requestsOf<ConnectRequest>().single.key, _hostKey);
      _expectNoSecretIn(
        _lines,
        where: 'pasting a valid key link, printed',
        secrets: _addHostSecrets,
      );
      _expectNoSecretIn(
        _shownTexts(tester),
        where: 'pasting a valid key link, shown text',
        secrets: _addHostSecrets,
      );
    });

    testWidgets(
      'bad links, a handoff link, an unknown key and a malformed key print and show no secret',
      (tester) async {
        final clipboard = _mockClipboard(tester);
        final store = HostStore(_MemoryBackend());
        // A host is held, so a typed key has an application key to be tried with.
        await store.addHost(name: _hostName, key: _hostKey, appKey: _appKey);
        await store.saveRelayKey(_relayKey);
        final host = FakeEngineHost();
        addTearDown(host.close);
        // The engine does not find the unknown key. Its refusal carries every secret and the marker as free text.
        host.onRequest = (request) {
          final unknown = request is ConnectRequest && request.key == _unfoundKey;
          host.deliver(
            encode(
              IpcReply(
                id: request.id,
                ok: !unknown,
                code: unknown ? 'HB-LOOKUP-TIMEOUT' : '',
                detail: unknown
                    ? 'search gave up: $_payloadMarker $_hostKey $_relayKey $_appKeyHex $_unfoundKey'
                    : '',
              ),
            ),
          );
        };
        final controller = AppController(host, store);
        addTearDown(controller.dispose);
        await controller.start();

        await _capturing(() async {
          _expectCaptureWorks();
          await tester.pumpWidget(
            MaterialApp(
              home: AddHostScreen(
                controller: controller,
                store: store,
                hostScreenBuilder: _hostScreenStub,
              ),
            ),
          );
          await tester.pumpAndSettle();

          // A key link with no application key after the dot.
          clipboard.text = _noAppKeyLink;
          await tester.tap(find.text('Paste link'));
          await _settle(tester);
          expect(find.text('HB-KEY-INVALID'), findsOneWidget, reason: 'the link is refused');
          _expectNoSecretIn(
            _shownTexts(tester),
            where: 'after a link with no application key',
            secrets: _addHostSecrets,
          );

          // A handoff link, which the add-host screen does not accept.
          clipboard.text = _handoffLink;
          await tester.tap(find.text('Paste link'));
          await _settle(tester);
          expect(find.text('HB-KEY-INVALID'), findsOneWidget, reason: 'a handoff link is refused');
          _expectNoSecretIn(
            _shownTexts(tester),
            where: 'after a handoff link',
            secrets: _addHostSecrets,
          );

          // A key link whose application key is upper case hex.
          clipboard.text = _badAppKeyLink;
          await tester.tap(find.text('Paste link'));
          await _settle(tester);
          expect(
            find.text('HB-APPKEY-INVALID'),
            findsOneWidget,
            reason: 'the application key is refused',
          );
          _expectNoSecretIn(
            _shownTexts(tester),
            where: 'after a bad application key',
            secrets: _addHostSecrets,
          );

          // A key the engine does not find: the screen shows the code of the failed try.
          await tester.enterText(find.byType(TextField), formatKey(_unfoundKey));
          await tester.tap(find.text('Add host'));
          await _settle(tester);
          expect(
            find.text('HB-LOOKUP-TIMEOUT'),
            findsOneWidget,
            reason: 'the engine did not find the key',
          );
          _expectNoSecretIn(
            _shownTexts(tester),
            where: 'after an unknown key',
            secrets: _addHostSecrets,
          );

          // A malformed key: a symbol outside the alphabet. Nothing is sent for it.
          await tester.enterText(find.byType(TextField), '7KQ-M4X-9TU');
          await tester.tap(find.text('Add host'));
          await _settle(tester);
          expect(
            find.text('HB-KEY-INVALID'),
            findsOneWidget,
            reason: 'the malformed key is refused',
          );
          _expectNoSecretIn(
            _shownTexts(tester),
            where: 'after a malformed key',
            secrets: _addHostSecrets,
          );
        });

        // The unknown key went to the engine, so the failed try was real.
        expect(host.requestsOf<ConnectRequest>().map((r) => r.key), contains(_unfoundKey));
        _expectNoSecretIn(_lines, where: 'the add-host steps, printed', secrets: _addHostSecrets);
      },
    );
  });

  group('the source', () {
    test('the app code uses no log channel that the capture cannot see', () {
      // The scan finds each channel in a sample, so a clean scan of the app means something.
      expect(_unseenLogChannelsIn("import 'dart:developer' as developer;"), ['dart:developer']);
      expect(_unseenLogChannelsIn("import 'package:logging/logging.dart';"), ['package:logging/']);
      expect(_unseenLogChannelsIn("import 'package:logger/logger.dart';"), ['package:logger/']);
      expect(_unseenLogChannelsIn("import 'package:flutter/foundation.dart';"), isEmpty);

      // flutter test runs with the package root, app/, as the working directory.
      final files = Directory('lib')
          .listSync(recursive: true)
          .whereType<File>()
          .where((file) => file.path.endsWith('.dart'))
          .toList();
      expect(files, isNotEmpty, reason: 'the scan reads the app code under lib/');

      // Each match names its file and channel, never a line, so no secret can reach the failure.
      final matches = [
        for (final file in files)
          for (final channel in _unseenLogChannelsIn(file.readAsStringSync()))
            '${file.path}: $channel',
      ];
      expect(
        matches,
        isEmpty,
        reason: 'these app files use a log channel the capture cannot see (file: channel)',
      );
    });
  });
}

/// Starts a controller that holds the host, its relay key and the events the screen reports, then copies
/// the diagnostics and reports a problem. The route carries every secret ([_routeWithSecrets]), so the copy
/// and the report have something to remove. The clipboard text and the issue link are then checked for every
/// secret, as the capture is for lines. The capture is expected to be set up by the caller.
Future<void> _copyAndReport(WidgetTester tester) async {
  _expectCaptureWorks();

  final copied = <String>[];
  tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (
    call,
  ) async {
    if (call.method == 'Clipboard.setData') {
      copied.add((call.arguments as Map)['text'] as String);
    }
    return null;
  });
  addTearDown(
    () => tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(
      SystemChannels.platform,
      null,
    ),
  );
  final opened = <Uri>[];

  final host = _answeringHost();
  addTearDown(host.close);
  final store = HostStore(_MemoryBackend());
  final controller = AppController(host, store);
  addTearDown(controller.dispose);
  await tester.runAsync(() async {
    final home = await store.addHost(name: _hostName, key: _hostKey, appKey: _appKey);
    await store.saveRelayKey(_relayKey);
    await controller.start();
    await controller.connect(home.id);
    host.deliver(encode(RouteEvent(host: _hostName, route: 'direct')));
    host.deliver(encode(LanEvent(host: _hostName, addresses: const [_lanAddress], port: 49152)));
    host.deliver(
      encode(
        ServicesEvent(
          host: _hostName,
          list: const [ServiceEntry(name: _serviceName, kind: 2)],
          ports: const [PortBinding(service: _serviceName, port: 8080)],
        ),
      ),
    );
    host.deliver(
      encode(ErrorEvent(code: 'HB-LOOKUP-TIMEOUT', detail: '$_payloadMarker $_hostKey')),
    );
    await pumpEventQueue();
  });

  await tester.pumpWidget(
    MaterialApp(
      home: DiagnosticsScreen(
        controller: controller,
        store: store,
        route: 'direct $_routeWithSecrets',
        nat: const NatInfo(host: _natAddress, port: 49737, firewalled: false, randomized: true),
        appVersion: '1.0.0+1',
        engineVersion: '3.1.4',
        protocolVersion: '1',
        openLink: (url) async => opened.add(url),
      ),
    ),
  );
  await tester.pumpAndSettle();

  await tester.tap(find.text('Copy diagnostics'));
  await tester.pumpAndSettle();
  await tester.tap(find.text('Report a problem'));
  await tester.pumpAndSettle();

  // The buttons ran, so the check covers the copy and the report.
  expect(copied, hasLength(1), reason: 'Copy diagnostics wrote the clipboard');
  expect(opened, hasLength(1), reason: 'Report a problem opened the issue');
  expect(copied.single, contains('direct'), reason: 'the copied report is not blank');

  // The clipboard text and the issue link hold no secret. The link is checked raw, decoded (a space is sent
  // as + and a + as %2B), and by each query value.
  _expectNoSecretIn(copied, where: 'the copied diagnostics');
  final link = opened.single.toString();
  _expectNoSecretIn([link], where: 'the report URL');
  _expectNoSecretIn([
    Uri.decodeComponent(link.replaceAll('+', ' ')),
  ], where: 'the report URL, decoded');
  _expectNoSecretIn(
    opened.single.queryParameters.values.toList(),
    where: 'the report query parameters',
  );
}
