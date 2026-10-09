import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/app_controller.dart';
import 'package:holebridge/src/diagnostics/redact.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';
import 'package:holebridge/src/ui/diagnostics_screen.dart';

import '../helpers/fake_engine_host.dart';

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

// Test values only (docs/security.md). They look real, so a leak would look like a real leak. The
// application key is 32 bytes; its hex and its base64 forms are both checked for.
final _appKey = Uint8List.fromList(List<int>.generate(32, (i) => (i * 37 + 11) & 0xff));
final _appKeyHex = _hex(_appKey);
final _appKeyBase64 = base64Encode(_appKey);
const _hostName = 'Home';
const _hostKey = '7KQM4X9TR';
const _relayKey = 'ZXVTSRQPN';
const _serviceName = 'photo-library';
const _lanAddress = '192.168.1.20';
// The NAT view's public address, from the documentation range (RFC 5737).
const _natAddress = '203.0.113.7';
const _nat = NatInfo(host: _natAddress, port: 49737, firewalled: false, randomized: true);
const _route = 'direct';
// The app version in pubspec form: its + must survive the URL encoding of the report body.
const _appVersion = '2.7.5+14';
const _engineVersion = '3.1.4';
const _protocolVersion = '1';
const _errorCode = 'HB-LOOKUP-TIMEOUT';

/// Every secret the fixture holds, in each form it takes, for checks on text. Checks ignore case.
final _secrets = <String, String>{
  'the host key': _hostKey,
  'the host key with dashes': '7KQ-M4X-9TR',
  'the application key in hex': _appKeyHex,
  'the application key in base64': _appKeyBase64,
  'the relay key': _relayKey,
  'the relay key with dashes': 'ZXV-TSR-QPN',
  'the service name': _serviceName,
  'the LAN address': _lanAddress,
  'the NAT public address': _natAddress,
};

/// A text that holds every secret of the fixture in every form it can take: bare, dashed, in lower case,
/// in upper case, twice, and inside key links. The screen tests below feed it through the fields that the
/// caller gives the report (the route, the engine version and the protocol version), so that the report
/// holds secrets before it is redacted and a missing or partial redaction shows.
final _dirty = [
  'host $_hostKey',
  'dashed 7KQ-M4X-9TR',
  'lower ${_hostKey.toLowerCase()}',
  'link https://holebridge.app/k#$_hostKey.$_appKeyHex',
  'dashed link https://holebridge.app/k#7KQ-M4X-9TR.$_appKeyHex',
  'app key $_appKeyHex',
  'upper ${_appKeyHex.toUpperCase()}',
  'base64 $_appKeyBase64',
  'relay $_relayKey',
  'dashed ZXV-TSR-QPN',
  'lower ${_relayKey.toLowerCase()}',
  'service $_serviceName and again $_serviceName',
  'lan $_lanAddress',
  'nat $_natAddress',
].join(' ');

String _hex(Uint8List bytes) => [for (final b in bytes) b.toRadixString(16).padLeft(2, '0')].join();

/// A fake engine that answers every request ok.
FakeEngineHost _answeringHost() {
  final host = FakeEngineHost();
  host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));
  return host;
}

/// A started controller and its store, holding the secrets and the state the screen reads.
class _Started {
  _Started(this.store, this.controller);

  final HostStore store;
  final AppController controller;
}

/// Starts an app whose state holds real-looking secrets: a host with its key and application key, a
/// relay key when [relay] is true, a service name and a LAN address reported by the engine, and the
/// last error code. The engine's events go through the controller, so the controller holds them too.
Future<_Started> _started(WidgetTester tester, {bool relay = true}) async {
  final started = await tester.runAsync(() async {
    final store = HostStore(_MemoryBackend());
    final added = await store.addHost(name: _hostName, key: _hostKey, appKey: _appKey);
    if (relay) await store.saveRelayKey(_relayKey);
    final host = _answeringHost();
    addTearDown(host.close);
    final controller = AppController(host, store);
    addTearDown(controller.dispose);
    await controller.start();
    host.deliver(encode(RouteEvent(host: _hostName, route: _route)));
    host.deliver(
      encode(
        ServicesEvent(
          host: _hostName,
          list: const [ServiceEntry(name: _serviceName, kind: 2)],
          ports: const [PortBinding(service: _serviceName, port: 8080)],
        ),
      ),
    );
    host.deliver(encode(LanEvent(host: _hostName, addresses: const [_lanAddress], port: 49152)));
    host.deliver(encode(ErrorEvent(code: _errorCode, detail: '')));
    await pumpEventQueue();

    // The fixture must hold what the tests rely on, or a passing check would prove nothing.
    expect(await store.servicesCache(added.id), [_serviceName], reason: 'fixture: service cached');
    expect(await store.lanAddresses(added.id), [_lanAddress], reason: 'fixture: LAN address saved');
    expect(controller.lastErrorCode, _errorCode, reason: 'fixture: controller error code set');
    expect(controller.view(added.id).route, _route, reason: 'fixture: host route set');
    return _Started(store, controller);
  });
  return started!;
}

/// Pumps the screen over [started] and lets what it loads from the store arrive. [openLink] receives the
/// report URL.
Future<void> _pumpScreen(
  WidgetTester tester,
  _Started started, {
  NatInfo? nat = _nat,
  String route = _route,
  String engineVersion = _engineVersion,
  String protocolVersion = _protocolVersion,
  Future<void> Function(Uri url)? openLink,
}) async {
  await tester.pumpWidget(
    MaterialApp(
      home: DiagnosticsScreen(
        controller: started.controller,
        store: started.store,
        route: route,
        nat: nat,
        appVersion: _appVersion,
        engineVersion: engineVersion,
        protocolVersion: protocolVersion,
        openLink: openLink ?? _ignoreLink,
      ),
    ),
  );
  await tester.pumpAndSettle();
}

Future<void> _ignoreLink(Uri url) async {}

/// Every text the screen shows, joined and lowercased, so a check does not depend on capitals.
String shownText(WidgetTester tester) {
  final parts = [
    for (final widget in tester.widgetList<Text>(find.byType(Text)))
      widget.data ?? widget.textSpan?.toPlainText() ?? '',
    for (final widget in tester.widgetList<SelectableText>(find.byType(SelectableText)))
      widget.data ?? widget.textSpan?.toPlainText() ?? '',
  ];
  return parts.join('\n').toLowerCase();
}

/// The first yes or no that follows the word relay in [text], or null when there is none. Other yes or no
/// answers on the screen (a NAT flag, say) do not count.
String? _relayAnswer(String text) => RegExp(r'relay[\s\S]*?\b(yes|no)\b').firstMatch(text)?[1];

/// Makes the test surface tall, so that the long texts of the redaction tests push no button out of sight.
void _tallSurface(WidgetTester tester) {
  tester.view.physicalSize = const Size(1000, 30000);
  tester.view.devicePixelRatio = 1.0;
  addTearDown(tester.view.reset);
}

/// Records what Copy diagnostics puts on the clipboard. Returns the list the texts are added to.
List<String> _recordClipboard(WidgetTester tester) {
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
  return copied;
}

/// Fails when [text] holds any secret of the fixture. The failure names the kind of secret, never
/// its value.
void _expectNoSecrets(String text, {required String what}) {
  final lower = text.toLowerCase();
  for (final entry in _secrets.entries) {
    expect(
      lower.contains(entry.value.toLowerCase()),
      isFalse,
      reason: '$what must not contain ${entry.key}',
    );
  }
}

void main() {
  // TEST CASE 1: the screen shows the route, the NAT type, whether a relay is set, the last error code
  // and the three versions.
  group('the diagnostics screen shows its facts', () {
    testWidgets('the route, NAT type, relay yes, last error code and the three versions', (
      tester,
    ) async {
      final started = await _started(tester);
      await _pumpScreen(tester, started);

      final text = shownText(tester);
      expect(text, contains(_route), reason: 'the route tried');
      expect(text, contains('randomized'), reason: 'the NAT type of a randomized NAT');
      expect(_relayAnswer(text), 'yes', reason: 'a relay key is set');
      expect(text, contains(_errorCode.toLowerCase()), reason: 'the last error code');
      expect(text, contains(_appVersion), reason: 'the app version');
      expect(text, contains(_engineVersion), reason: 'the engine version');
      expect(
        RegExp(r'protocol\D{0,20}(?<!\d)1(?!\d)').hasMatch(text),
        isTrue,
        reason: 'the protocol version after its label',
      );
    });

    testWidgets('relay no when no relay key is set', (tester) async {
      final started = await _started(tester, relay: false);
      await _pumpScreen(tester, started);

      final text = shownText(tester);
      expect(_relayAnswer(text), 'no', reason: 'no relay key is set');
    });
  });

  // TEST CASE 2: the copied text contains none of the host key, the app key, the relay key, a service
  // name or a LAN address, although the controller state holds them all.
  testWidgets(
    'the copied text holds no host key, application key, relay key, service name or LAN address',
    (tester) async {
      final started = await _started(tester);
      final copied = _recordClipboard(tester);
      await _pumpScreen(tester, started);

      await tester.tap(find.text('Copy diagnostics'));
      await tester.pumpAndSettle();

      final text = copied.single;
      // The copy carries the diagnostics, so the checks below do not pass on an empty report.
      expect(text.toLowerCase(), contains('randomized'), reason: 'the NAT type is copied');
      expect(
        text.toLowerCase(),
        contains(_errorCode.toLowerCase()),
        reason: 'the error code is copied',
      );
      expect(text, contains(_appVersion), reason: 'the app version is copied');
      _expectNoSecrets(text, what: 'the copied text');
    },
  );

  // TEST CASE 3: the report URL's body is redacted the same way and is URL-encoded.
  testWidgets('Report a problem opens the issue with a redacted, URL-encoded body', (tester) async {
    final started = await _started(tester);
    final opened = <Uri>[];
    await _pumpScreen(tester, started, openLink: (url) async => opened.add(url));

    await tester.tap(find.text('Report a problem'));
    await tester.pumpAndSettle();

    final url = opened.single;
    final raw = url.toString();
    expect(raw, startsWith('https://github.com/andrewloable/HoleBridge/issues/new'));
    expect(url.queryParameters['title'], isNotEmpty, reason: 'the title is prefilled');

    // URL-encoded: no raw space or line break is left in the URL.
    expect(raw, isNot(contains(' ')), reason: 'spaces are percent-encoded');
    expect(raw, isNot(contains('\n')), reason: 'line breaks are percent-encoded');

    final body = url.queryParameters['body'] ?? '';
    expect(body, isNotEmpty, reason: 'the body is prefilled');
    if (body.contains('\n')) {
      expect(raw, contains('%0A'), reason: 'the line breaks of the body are encoded');
    }
    expect(body.toLowerCase(), contains('randomized'), reason: 'the NAT type is in the body');
    expect(
      body.toLowerCase(),
      contains(_errorCode.toLowerCase()),
      reason: 'the error code is in the body',
    );
    // A + that is not encoded would read back as a space, so the decoded body shows whether it was encoded.
    expect(body, contains(_appVersion), reason: 'the app version reads back intact from the body');

    // Redacted before it is encoded: neither the decoded body nor the URL holds a secret.
    _expectNoSecrets(body, what: 'the report body');
    _expectNoSecrets(raw, what: 'the report URL');
    // The raw URL holds the base64 form of the application key only as %2B, %2F and %3D, so check the
    // decoded URL too, and the title, which is no part of the body.
    _expectNoSecrets(url.queryParameters['title'] ?? '', what: 'the report title');
    _expectNoSecrets(Uri.decodeComponent(raw.replaceAll('+', ' ')), what: 'the decoded report URL');
  });

  // The fixture puts no secret into the report by itself, so the tests above also pass for a screen that
  // never calls redact. Here the fields that the caller gives the report (route, engine version, protocol
  // version) carry every secret in every form, and both ways out must remove all of them.
  group('the screen redacts whatever the report prints', () {
    final route = '$_route $_dirty';
    final engineVersion = '$_engineVersion $_dirty';
    final protocolVersion = '$_protocolVersion $_dirty';

    testWidgets('Copy diagnostics removes every secret in every form', (tester) async {
      _tallSurface(tester);
      final started = await _started(tester);
      final copied = _recordClipboard(tester);
      await _pumpScreen(
        tester,
        started,
        route: route,
        engineVersion: engineVersion,
        protocolVersion: protocolVersion,
      );

      await tester.tap(find.text('Copy diagnostics'));
      await tester.pumpAndSettle();

      final text = copied.single;
      expect(text, contains(_appVersion), reason: 'the report is copied, not blanked');
      expect(text, contains(_engineVersion), reason: 'the engine version is copied');
      expect(
        text.toLowerCase(),
        contains(_errorCode.toLowerCase()),
        reason: 'the error code is copied',
      );
      _expectNoSecrets(text, what: 'the copied text');
    });

    testWidgets('Report a problem removes every secret from the title, the body and the URL', (
      tester,
    ) async {
      _tallSurface(tester);
      final started = await _started(tester);
      final opened = <Uri>[];
      await _pumpScreen(
        tester,
        started,
        route: route,
        engineVersion: engineVersion,
        protocolVersion: protocolVersion,
        openLink: (url) async => opened.add(url),
      );

      await tester.tap(find.text('Report a problem'));
      await tester.pumpAndSettle();

      final url = opened.single;
      final raw = url.toString();
      expect(url.queryParameters['body'], contains(_appVersion), reason: 'the body is the report');
      expect(url.queryParameters['body'], contains(_engineVersion), reason: 'the engine version');
      for (final entry in url.queryParameters.entries) {
        _expectNoSecrets(entry.value, what: 'the report parameter ${entry.key}');
      }
      _expectNoSecrets(raw, what: 'the report URL');
      _expectNoSecrets(Uri.decodeComponent(raw.replaceAll('+', ' ')), what: 'the decoded report URL');
    });
  });

  // TEST CASE 4: redact replaces any IPv4 address and any 9-symbol key pattern, even when the app does
  // not know them.
  group('redact removes what the app does not know', () {
    test('an IPv4 address is replaced', () {
      final out = redact('dialled 198.51.100.23 then 192.168.1.20', const []);
      expect(out, isNot(contains('198.51.100.23')));
      expect(out, isNot(contains('192.168.1.20')));
    });

    test('an IPv6 address is replaced, compressed or full', () {
      final out = redact('peer 2001:db8::1 and 2001:0db8:85a3:0000:0000:8a2e:0370:7334', const []);
      expect(out, isNot(contains('2001:db8::1')));
      expect(out, isNot(contains('2001:0db8:85a3:0000:0000:8a2e:0370:7334')));
    });

    test('a 9-symbol key pattern is replaced, dashed, bare and in lower case', () {
      const keys = ['3HZ-8RW-5K2', '3HZ8RW5K2', '3hz-8rw-5k2'];
      final out = redact('keys ${keys.join(' ')} end', const []);
      for (final key in keys) {
        expect(
          out.toLowerCase(),
          isNot(contains(key.toLowerCase())),
          reason: 'a key pattern stays',
        );
      }
    });

    test('text that is not a secret is left as it is', () {
      const text = 'HB-LOOKUP-TIMEOUT on app 2.7.5, route direct';
      expect(redact(text, const []), text);
    });

    test('the known secrets are removed even where no pattern matches them', () {
      final out = redact('service garage-nas on 10.0.0.5 with app key $_appKeyHex', [
        'garage-nas',
        '10.0.0.5',
        _appKeyHex,
      ]);
      expect(out, isNot(contains('garage-nas')));
      expect(out, isNot(contains('10.0.0.5')));
      expect(out, isNot(contains(_appKeyHex)));
    });
  });

  // redact removes a secret in every form and every place it can take, not only the form it is stored in.
  group('redact removes a secret in every form', () {
    test('a host key is removed bare, dashed, in lower case and inside a key link', () {
      const link = 'https://holebridge.app/k';
      final forms = {
        'bare': _hostKey,
        'dashed': '7KQ-M4X-9TR',
        'lower case': _hostKey.toLowerCase(),
        'lower case dashed': '7kq-m4x-9tr',
        'a key link': '$link#$_hostKey.$_appKeyHex',
        'a key link, dashed': '$link#7KQ-M4X-9TR.$_appKeyHex',
        'a key link, lower case': '$link#${_hostKey.toLowerCase()}.$_appKeyHex',
        'a key link, dashes percent-encoded': '$link#7KQ%2DM4X%2D9TR.$_appKeyHex',
      };
      for (final entry in forms.entries) {
        // Only the stored forms are known, as the store holds them: the normalized key and the hex.
        final out = redact('see ${entry.value} stays?', [_hostKey, _appKeyHex]).toLowerCase();
        expect(out, isNot(contains('7kqm4x9tr')), reason: 'the key stays, form: ${entry.key}');
        expect(out, isNot(contains('7kq-m4x-9tr')), reason: 'the key stays, form: ${entry.key}');
        expect(out, isNot(contains('7kq%2dm4x%2d9tr')), reason: 'the key stays, form: ${entry.key}');
        expect(out, isNot(contains(_appKeyHex)), reason: 'the app key stays, form: ${entry.key}');
        expect(out, contains('stays?'), reason: 'the text around it stays, form: ${entry.key}');
      }
    });

    test('a secret is removed in upper case too', () {
      final upper = _appKeyHex.toUpperCase();
      final out = redact('app key $upper on ${_serviceName.toUpperCase()}', [
        _appKeyHex,
        _serviceName,
      ]);
      expect(out.toLowerCase(), isNot(contains(_appKeyHex)));
      expect(out.toLowerCase(), isNot(contains(_serviceName)));
    });

    test('a secret that appears twice is removed both times', () {
      final out = redact(
        '$_serviceName up, $_serviceName down, key $_appKeyHex then $_appKeyHex',
        [_serviceName, _appKeyHex],
      );
      expect(out, isNot(contains(_serviceName)));
      expect(out, isNot(contains(_appKeyHex)));
    });

    test('a secret that is part of another secret is removed whole, in either order', () {
      for (final secrets in [
        ['library', 'photo-library'],
        ['photo-library', 'library'],
      ]) {
        final out = redact('service photo-library is up', secrets).toLowerCase();
        expect(out, isNot(contains('photo')), reason: 'a part of the name stays, order $secrets');
        expect(out, isNot(contains('library')), reason: 'a part of the name stays, order $secrets');
      }
    });

    test('a secret is matched as it is written, whatever characters it holds', () {
      const secrets = ['nas (backup)', 'media [4k', 'a+b', 'x|y', 'AB+cd/EF9g==', 'photos.old'];
      for (final secret in secrets) {
        final out = redact('before $secret after', [secret]);
        expect(out, isNot(contains(secret)), reason: 'a secret with special characters stays');
        expect(out, contains('before'));
        expect(out, contains('after'));
      }
    });

    test('an empty secret changes nothing', () {
      const text = 'route direct, app 2.7.5';
      expect(redact(text, ['', _serviceName]), text);
    });
  });
}
