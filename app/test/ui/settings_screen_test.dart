// Widget tests for the settings screen (docs/cli.md#the-app, Settings). The screen is driven through a real
// AppController over a FakeEngineHost, as the host screen's tests are (test/ui/host_screen_test.dart): the
// controller owns every engine request, so the screen sends none itself, and these tests assert the
// requests that reach the fake engine.

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/app_controller.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';
import 'package:holebridge/src/ui/settings_screen.dart';
import 'package:holebridge/src/vpn/vpn_mode_control.dart';

import '../helpers/fake_engine_host.dart';

// The controls carry these keys, so a test finds each one without depending on its text. The IMPL task
// (HoleBridge-5vk.14) puts them on the controls. The key of a share switch is per host.
const relayKeyFieldKey = Key('relay-key-field');
const relaySaveKey = Key('relay-save');
const vpnModeSwitchKey = Key('vpn-mode-switch');
const diagnosticsRowKey = Key('diagnostics-row');
const shareWarningConfirmKey = Key('share-warning-confirm');
const shareWarningCancelKey = Key('share-warning-cancel');
Key shareSwitchKey(String hostId) => Key('share-$hostId');

// Test values only (docs/security.md): an application key of 32 bytes, and 9-symbol keys in the
// Crockford alphabet. The typed relay key has lower case and dashes, and normalizes to _relayKey.
final _appKey = Uint8List.fromList(List<int>.generate(32, (i) => i));
const _hostName = 'Living room';
const _hostKey = '7KQM4X9TR';
const _relayTyped = 'pqr-stv-wxy';
const _relayKey = 'PQRSTVWXY';
const _storedRelayKey = 'ZXVTSRQPN';
// U is outside the Crockford alphabet, so this typed key does not normalize.
const _invalidRelayTyped = 'PQR-STV-WXU';

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

/// VPN mode that starts off. The settings screen only asks it for its state and its switch.
class _FakeVpnMode implements VpnModeControl {
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

/// What one test drives: the fake engine, the store with one host, the controller started over the engine,
/// and the id of the host.
class _Harness {
  _Harness(this.host, this.store, this.controller, this.hostId);

  final FakeEngineHost host;
  final HostStore store;
  final AppController controller;
  final String hostId;
}

/// Starts a controller over a fake engine that answers every request ok, with one stored host that the
/// controller has connected, so the engine has it registered. [shared] stores Share with my network as on
/// for the host before the controller starts, so the host connects with bind 0.0.0.0. [relayKey] stores a
/// relay key before the controller starts. The requests that start() and connect() sent are forgotten, so
/// the test sees only the requests the screen causes.
Future<_Harness> _started({bool shared = false, String? relayKey}) async {
  final store = HostStore(_MemoryBackend());
  final added = await store.addHost(name: _hostName, key: _hostKey, appKey: _appKey);
  if (shared) await store.saveShared(added.id, true);
  if (relayKey != null) await store.saveRelayKey(relayKey);
  final host = FakeEngineHost();
  addTearDown(host.close);
  host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));
  final controller = AppController(host, store);
  addTearDown(controller.dispose);
  await controller.start();
  await controller.connect(added.id);
  host.sent.clear();
  return _Harness(host, store, controller, added.id);
}

/// Pumps the settings screen over [h] and fails at once if it does not build, so a build error is the
/// failure that is reported, not the finders that run after it. The surface is tall, so every control is
/// on screen and a tap needs no scrolling.
Future<void> _pumpSettings(
  WidgetTester tester,
  _Harness h, {
  VoidCallback? onOpenDiagnostics,
}) async {
  tester.view.physicalSize = const Size(800, 1600);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.resetPhysicalSize);
  addTearDown(tester.view.resetDevicePixelRatio);
  await tester.pumpWidget(
    MaterialApp(
      home: SettingsScreen(
        controller: h.controller,
        store: h.store,
        vpn: _FakeVpnMode(),
        onOpenDiagnostics: onOpenDiagnostics ?? () {},
      ),
    ),
  );
  expect(tester.takeException(), isNull, reason: 'the settings screen builds');
  await tester.pumpAndSettle();
}

/// Taps the control with [key], then lets the screen settle.
Future<void> _tap(WidgetTester tester, Key key) async {
  await tester.tap(find.byKey(key));
  await tester.pumpAndSettle();
}

/// The error the text field with [key] shows, or null when it shows none. The key may sit on the
/// TextField or on a widget that holds one.
String? _fieldError(WidgetTester tester, Key key) {
  final inside = find.descendant(of: find.byKey(key), matching: find.byType(TextField));
  final field = inside.evaluate().isNotEmpty ? inside : find.byKey(key);
  return tester.widget<TextField>(field).decoration?.errorText;
}

/// Whether the switch with [key] is on. The key may sit on the Switch or on a tile that holds one.
bool _switchOn(WidgetTester tester, Key key) {
  final inside = find.descendant(of: find.byKey(key), matching: find.byType(Switch));
  final switchFinder = inside.evaluate().isNotEmpty ? inside : find.byKey(key);
  return tester.widget<Switch>(switchFinder).value;
}

/// Expects that the engine got exactly a close for the host and then a connect for it with [bind]. The
/// engine gives a connect for a host it already has HB-USAGE (spec/ipc.md, connect), so a new bind needs
/// the close first.
void _expectReconnect(_Harness h, String bind) {
  final requests = h.host.requests;
  expect(requests, hasLength(2), reason: 'a close and a connect, nothing else');
  expect(requests[0], isA<CloseRequest>(), reason: 'the host is closed first');
  expect((requests[0] as CloseRequest).host, _hostName);
  expect(requests[1], isA<ConnectRequest>(), reason: 'then connected again');
  final connect = requests[1] as ConnectRequest;
  expect(connect.host, _hostName);
  expect(connect.key, _hostKey);
  expect(connect.appKey, _appKey);
  expect(connect.bind, bind);
}

void main() {
  group('SettingsScreen', () {
    testWidgets('saving a relay key normalizes it, stores it and sends relay with it', (
      tester,
    ) async {
      final h = await _started();
      await _pumpSettings(tester, h);

      await tester.enterText(find.byKey(relayKeyFieldKey), _relayTyped);
      await _tap(tester, relaySaveKey);

      expect(await h.store.relayKey(), _relayKey, reason: 'the stored key is normalized');
      final relay = h.host.requestsOf<RelayRequest>().single;
      expect(relay.key, _relayKey);
      expect(relay.appKey, _appKey, reason: 'relay carries the held application key');
    });

    testWidgets('an invalid relay key shows an error and sends nothing', (tester) async {
      final h = await _started();
      await _pumpSettings(tester, h);

      await tester.enterText(find.byKey(relayKeyFieldKey), _invalidRelayTyped);
      await _tap(tester, relaySaveKey);

      expect(_fieldError(tester, relayKeyFieldKey), isNotNull, reason: 'the field shows an error');
      expect(h.host.requests, isEmpty, reason: 'no request goes to the engine');
      expect(await h.store.relayKey(), isNull, reason: 'nothing is stored');
    });

    testWidgets(
      'Share with my network shows the warning; cancelling leaves it off; confirming turns it on and reconnects with bind 0.0.0.0',
      (tester) async {
        final h = await _started();
        await _pumpSettings(tester, h);

        // Off by default. Turning it on shows the warning dialog first.
        expect(_switchOn(tester, shareSwitchKey(h.hostId)), isFalse);
        await _tap(tester, shareSwitchKey(h.hostId));
        expect(find.byType(AlertDialog), findsOneWidget, reason: 'the warning dialog shows');

        // Cancelling leaves it off and sends nothing.
        await _tap(tester, shareWarningCancelKey);
        expect(find.byType(AlertDialog), findsNothing);
        expect(_switchOn(tester, shareSwitchKey(h.hostId)), isFalse);
        expect(h.host.requests, isEmpty, reason: 'a cancelled warning sends nothing');
        expect(await h.store.shared(h.hostId), isFalse, reason: 'a cancelled warning stores nothing');

        // Confirming turns it on, stores it, and connects the host again with bind 0.0.0.0.
        await _tap(tester, shareSwitchKey(h.hostId));
        await _tap(tester, shareWarningConfirmKey);
        expect(_switchOn(tester, shareSwitchKey(h.hostId)), isTrue);
        expect(await h.store.shared(h.hostId), isTrue);
        _expectReconnect(h, '0.0.0.0');
      },
    );

    // Not a TEST CASE of HoleBridge-5vk.13: turning Share with my network off follows from its DESIGN
    // (off by default, per host). A switch that went off without the reconnect would leave the host
    // listening on every interface.
    testWidgets(
      'turning Share with my network off needs no warning and connects the host again with bind 127.0.0.1',
      (tester) async {
        final h = await _started(shared: true);
        await _pumpSettings(tester, h);

        expect(_switchOn(tester, shareSwitchKey(h.hostId)), isTrue, reason: 'the stored setting shows');
        await _tap(tester, shareSwitchKey(h.hostId));

        expect(find.byType(AlertDialog), findsNothing, reason: 'turning it off needs no warning');
        expect(_switchOn(tester, shareSwitchKey(h.hostId)), isFalse);
        expect(await h.store.shared(h.hostId), isFalse);
        _expectReconnect(h, '127.0.0.1');
      },
    );

    testWidgets(
      'the VPN mode switch is visible on Android and hidden on desktop (platform override)',
      (tester) async {
        final h = await _started();

        // The override is cleared in a finally block, not in addTearDown: the framework checks that no
        // debug variable is left set before the tear-down callbacks run.
        try {
          debugDefaultTargetPlatformOverride = TargetPlatform.android;
          await _pumpSettings(tester, h);
          expect(find.byKey(vpnModeSwitchKey), findsOneWidget, reason: 'Android shows VPN mode');

          // Every other platform hides it: desktop, and iOS until M5. The screen is built again each
          // time, so the platform is read again.
          for (final platform in TargetPlatform.values.where((p) => p != TargetPlatform.android)) {
            debugDefaultTargetPlatformOverride = platform;
            await tester.pumpWidget(const SizedBox.shrink());
            await _pumpSettings(tester, h);
            expect(
              find.byKey(vpnModeSwitchKey),
              findsNothing,
              reason: '${platform.name} hides VPN mode',
            );
          }
        } finally {
          debugDefaultTargetPlatformOverride = null;
        }
      },
    );

    // Not a TEST CASE of HoleBridge-5vk.13: this comes from the task's DESIGN field.
    testWidgets('clearing the relay key sends an empty relay and removes the stored key', (
      tester,
    ) async {
      final h = await _started(relayKey: _storedRelayKey);
      await _pumpSettings(tester, h);

      await tester.enterText(find.byKey(relayKeyFieldKey), '');
      await _tap(tester, relaySaveKey);

      expect(await h.store.relayKey(), isNull, reason: 'the stored key is removed');
      final relay = h.host.requestsOf<RelayRequest>().single;
      expect(relay.key, '', reason: 'an empty key means no relay');
      expect(relay.appKey, hasLength(32), reason: 'the application key is 32 bytes, all zero');
      expect(relay.appKey, everyElement(0));
    });

    // Not a TEST CASE of HoleBridge-5vk.13: this comes from the task's DESIGN field.
    testWidgets('the Diagnostics row opens the diagnostics screen', (tester) async {
      final h = await _started();
      var opened = 0;
      await _pumpSettings(
        tester,
        h,
        onOpenDiagnostics: () {
          opened++;
        },
      );

      await _tap(tester, diagnosticsRowKey);

      expect(opened, 1);
    });
  });
}
