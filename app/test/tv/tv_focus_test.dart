// Widget tests for D-pad focus on every screen (docs/architecture.md#platforms, Android TV; HoleBridge-hb5.4.1).
// Each test presses the remote's keys (the arrows, Select) or Back, and checks where focus lands and what the
// key does. The screens are driven as their own tests drive them: a real AppController over a FakeEngineHost.
// Every screen runs under TvFocusScope, as the app wraps its navigator in it.

import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/app_controller.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';
import 'package:holebridge/src/keys/normalize.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';
import 'package:holebridge/src/tv/tv_focus.dart';
import 'package:holebridge/src/ui/add_host_screen.dart';
import 'package:holebridge/src/ui/diagnostics_screen.dart';
import 'package:holebridge/src/ui/host_screen.dart';
import 'package:holebridge/src/ui/key_entry_field.dart';
import 'package:holebridge/src/ui/service_tile.dart';
import 'package:holebridge/src/ui/settings_screen.dart';
import 'package:holebridge/src/vpn/vpn_mode_control.dart';

import '../helpers/fake_engine_host.dart';

// The settings screen carries this key on the Diagnostics row (lib/src/ui/settings_screen.dart). The
// settings tests give the same value (test/ui/settings_screen_test.dart).
const _diagnosticsRowKey = Key('diagnostics-row');

// Test values only (docs/security.md): 9-symbol keys in the Crockford alphabet and a 32-byte application key.
const _hostName = 'living-room';
const _keyA = '7KQM4X9TR';
const _otherKey = 'HJKMNPQRS';
const _typedKey = '7kq-m4x-9tr';
// A key link with no application key after the dot: Paste link shows HB-KEY-INVALID for it.
const _invalidLink = 'https://holebridge.app/k#$_keyA';
final _appKey = Uint8List.fromList(List<int>.generate(32, (i) => i));

/// A tcp service and a udp service. Each tile offers Copy address, and no other button that is enabled.
const _services = [ServiceEntry(name: 'ssh', kind: 3), ServiceEntry(name: 'dns', kind: 4)];
const _ports = [PortBinding(service: 'ssh', port: 53817), PortBinding(service: 'dns', port: 5353)];

const _rootText = 'Root page';

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

/// Records each addTypedKey call and does not connect, as the add-host screen's tests do.
class _RecordingController extends AppController {
  _RecordingController(super.host, super.store);

  final List<String> typedKeys = [];

  @override
  Future<String?> addTypedKey(String typed) async {
    typedKeys.add(typed);
    return null;
  }
}

/// VPN mode that starts off, as the settings screen's tests fake it.
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

/// Starts a controller over a fake engine with one stored host, whose cached services are [_services], so the
/// host screen has its tiles before any session is up. The engine refuses every request with
/// HB-VERSION-MISMATCH, as the host screen's tests do, so the connects the screens send change no tile.
Future<_Harness> _started() async {
  final store = HostStore(_MemoryBackend());
  final added = await store.addHost(name: _hostName, key: _keyA, appKey: _appKey);
  await store.saveServices(added.id, [for (final service in _services) service.name]);
  await store.saveServiceKinds(added.id, {
    for (final service in _services) service.name: service.kind,
  });
  final host = FakeEngineHost();
  addTearDown(host.close);
  host.onRequest = (request) =>
      host.deliver(encode(IpcReply(id: request.id, ok: false, code: 'HB-VERSION-MISMATCH')));
  final controller = AppController(host, store);
  addTearDown(controller.dispose);
  await controller.start();
  return _Harness(host, store, controller, added.id);
}

/// A controller that holds an application key, so a typed key is tried, over an engine that answers ok. The
/// store is returned with it, for the add-host screen.
Future<(HostStore, _RecordingController)> _addHostHarness() async {
  final host = FakeEngineHost();
  addTearDown(host.close);
  host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));
  final store = HostStore(_MemoryBackend());
  // Adding a host holds its application key, so a typed key has one to be tried with.
  await store.addHost(name: 'Home', key: _otherKey, appKey: _appKey);
  final controller = _RecordingController(host, store);
  addTearDown(controller.dispose);
  await controller.start();
  return (store, controller);
}

/// The page under test, in a MaterialApp whose navigator is wrapped in TvFocusScope, as the app wraps it.
Widget _app(Widget home) => MaterialApp(
  builder: (context, child) => TvFocusScope(child: child!),
  home: home,
);

/// The page under which the screens are pushed, so Back has a route to return to.
Widget _rootPage() => _app(const Scaffold(body: Center(child: Text(_rootText))));

/// The add-host screen for a TV, which has no camera: Add from phone is its primary action.
Widget _tvAddHost(HostStore store, AppController controller, VoidCallback onAddFromPhone) {
  return AddHostScreen(
    controller: controller,
    store: store,
    hostScreenBuilder: (context, hostId) => const SizedBox(),
    isTv: true,
    onAddFromPhone: onAddFromPhone,
  );
}

HostScreen _hostScreen(_Harness h) =>
    HostScreen(controller: h.controller, store: h.store, hostId: h.hostId);

SettingsScreen _settings(_Harness h, {VoidCallback? onOpenDiagnostics}) => SettingsScreen(
  controller: h.controller,
  store: h.store,
  vpn: _FakeVpnMode(),
  onOpenDiagnostics: onOpenDiagnostics ?? () {},
);

DiagnosticsScreen _diagnostics(_Harness h) => DiagnosticsScreen(
  controller: h.controller,
  store: h.store,
  route: 'lan',
  nat: null,
  appVersion: '1.0.0',
  engineVersion: '0.0.0',
  protocolVersion: '1',
  openLink: _ignoreLink,
);

Future<void> _ignoreLink(Uri url) async {}

/// A surface tall enough for every tile, so each one is built and can be reached.
void _tallSurface(WidgetTester tester) {
  tester.view.physicalSize = const Size(800, 1600);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.resetPhysicalSize);
  addTearDown(tester.view.resetDevicePixelRatio);
}

/// Pumps [widget]. A build error is thrown at once, so the error is the failure that is reported and not the
/// finders that run after it, as the screens' own tests report it.
Future<void> _pump(WidgetTester tester, Widget widget) async {
  await tester.pumpWidget(widget);
  final error = tester.takeException();
  if (error != null) throw error;
}

/// Pumps the host screen and lets its tiles build.
Future<void> _pumpHostScreen(WidgetTester tester, _Harness h) async {
  await _pump(tester, _app(_hostScreen(h)));
  await tester.pumpAndSettle();
}

/// Presses one key of the remote and lets the frame run.
Future<void> _press(WidgetTester tester, LogicalKeyboardKey key) async {
  await tester.sendKeyEvent(key);
  await tester.pump();
}

/// Presses Back, as the remote's Back key does: the platform asks the app to pop its route.
Future<void> _back(WidgetTester tester) async {
  await tester.binding.handlePopRoute();
  await tester.pumpAndSettle();
}

/// Pushes [screen] over the page under test, as the app's navigator pushes a route.
Future<void> _push(WidgetTester tester, Widget screen) async {
  // The push completes when the route is popped, so it is not awaited here.
  unawaited(
    tester
        .state<NavigatorState>(find.byType(Navigator))
        .push(MaterialPageRoute<void>(builder: (_) => screen)),
  );
  await tester.pumpAndSettle();
}

/// Presses the keys in [keys] in turn, one press each, until [reached] is true. It fails when [reached] is
/// still false after [maxPresses] presses. [what] names the target in the failure.
Future<void> _navigateUntil(
  WidgetTester tester,
  bool Function() reached,
  String what, {
  List<LogicalKeyboardKey> keys = const [LogicalKeyboardKey.arrowDown],
  int maxPresses = 8,
}) async {
  for (var press = 0; press < maxPresses && !reached(); press++) {
    await _press(tester, keys[press % keys.length]);
  }
  expect(reached(), isTrue, reason: 'the remote reaches $what');
}

/// Whether the primary focus is on an element of [target], or inside one. A button inside a tile is inside
/// the tile, so a check that means the tile itself also needs the tile's button not to have focus (see
/// [_onTile]).
bool _focusWithin(Finder target) {
  final focused = FocusManager.instance.primaryFocus?.context;
  if (focused == null) return false;
  for (final match in target.evaluate()) {
    if (identical(match, focused)) return true;
    var within = false;
    focused.visitAncestorElements((ancestor) {
      if (identical(ancestor, match)) within = true;
      return !within;
    });
    if (within) return true;
  }
  return false;
}

/// The tile of the service [name]: the ServiceTile that holds the name's text.
Finder _tile(String name) => find.ancestor(of: find.text(name), matching: find.byType(ServiceTile));

/// The Copy address button of the tile of [name].
Finder _copy(String name) =>
    find.descendant(of: _tile(name), matching: find.widgetWithText(TextButton, 'Copy address'));

/// Whether focus is on the tile of [name] itself, not on one of its buttons.
bool _onTile(String name) => _focusWithin(_tile(name)) && !_focusWithin(_copy(name));

/// Fakes the clipboard for one test: getData returns [paste], and the text of each setData is recorded. The
/// returned function gives the text copied last.
String? Function() _fakeClipboard(WidgetTester tester, {String paste = ''}) {
  String? copied;
  tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (
    call,
  ) async {
    if (call.method == 'Clipboard.getData') return <String, dynamic>{'text': paste};
    if (call.method == 'Clipboard.setData') copied = (call.arguments as Map)['text'] as String?;
    return null;
  });
  addTearDown(
    () => tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(
      SystemChannels.platform,
      null,
    ),
  );
  return () => copied;
}

/// The text the key field shows.
String _fieldText(WidgetTester tester) =>
    tester.widget<EditableText>(find.byType(EditableText)).controller.text;

/// The caret offset in the key field.
int _caret(WidgetTester tester) =>
    tester.widget<EditableText>(find.byType(EditableText)).controller.selection.baseOffset;

void main() {
  group('AddHostScreen on a TV', () {
    testWidgets('opens with focus on its primary action, Add from phone', (tester) async {
      final (store, controller) = await _addHostHarness();

      await _pump(tester, _app(_tvAddHost(store, controller, () {})));
      await tester.pumpAndSettle();

      expect(
        _focusWithin(find.widgetWithText(FilledButton, 'Add from phone')),
        isTrue,
        reason: 'the first focus lands on Add from phone',
      );
    });

    // Test case 1: arrow-down moves focus through the options in order, and select activates the focused one.
    testWidgets(
      'arrow-down moves focus through the options in order, and select activates the focused one',
      (tester) async {
        final (store, controller) = await _addHostHarness();
        _fakeClipboard(tester, paste: _invalidLink);
        var addFromPhone = 0;
        await _pump(
          tester,
          _app(
            _tvAddHost(store, controller, () {
              addFromPhone++;
            }),
          ),
        );
        await tester.pumpAndSettle();

        await _press(tester, LogicalKeyboardKey.select);
        expect(
          addFromPhone,
          1,
          reason: 'select on the focused Add from phone calls onAddFromPhone',
        );

        // Paste link is on the same row as Add from phone, to its left.
        await _press(tester, LogicalKeyboardKey.arrowLeft);
        expect(
          _focusWithin(find.widgetWithText(OutlinedButton, 'Paste link')),
          isTrue,
          reason: 'arrow-left reaches Paste link',
        );
        await _press(tester, LogicalKeyboardKey.select);
        await tester.pumpAndSettle();
        expect(
          find.textContaining('HB-KEY-INVALID'),
          findsWidgets,
          reason: 'select on Paste link reads the clipboard and shows its error',
        );

        await _press(tester, LogicalKeyboardKey.arrowDown);
        expect(
          _focusWithin(find.byType(KeyEntryField)),
          isTrue,
          reason: 'arrow-down from Paste link reaches the typed key',
        );
        await tester.enterText(find.byType(TextField), _typedKey);
        await tester.pump();

        await _press(tester, LogicalKeyboardKey.arrowDown);
        expect(
          _focusWithin(find.widgetWithText(FilledButton, 'Add host')),
          isTrue,
          reason: 'arrow-down from the typed key reaches Add host',
        );
        await _press(tester, LogicalKeyboardKey.select);
        await tester.pumpAndSettle();
        expect(controller.typedKeys, hasLength(1), reason: 'select on Add host adds the typed key');
        expect(normalizeKey(controller.typedKeys.single), _keyA);
      },
    );
  });

  group('the host screen', () {
    // Test case 2: arrows move between tiles and their buttons, and select presses.
    testWidgets('arrows move between tiles and their buttons, and select presses', (tester) async {
      _tallSurface(tester);
      final h = await _started();
      await _pumpHostScreen(tester, h);
      h.host.deliver(encode(ServicesEvent(host: _hostName, list: _services, ports: _ports)));
      await tester.pumpAndSettle();
      final copied = _fakeClipboard(tester);

      await _navigateUntil(tester, () => _onTile('ssh'), 'the ssh tile');
      await _press(tester, LogicalKeyboardKey.arrowDown);
      expect(
        _focusWithin(_copy('ssh')),
        isTrue,
        reason: 'arrow-down from the ssh tile reaches its Copy address',
      );
      await _press(tester, LogicalKeyboardKey.select);
      await tester.pumpAndSettle();
      expect(copied(), '127.0.0.1:53817', reason: 'select on Copy address copies the address');

      await _press(tester, LogicalKeyboardKey.arrowDown);
      expect(
        _onTile('dns'),
        isTrue,
        reason: 'arrow-down from the ssh button reaches the next tile',
      );
      await _press(tester, LogicalKeyboardKey.arrowDown);
      expect(
        _focusWithin(_copy('dns')),
        isTrue,
        reason: 'arrow-down from the dns tile reaches its Copy address',
      );
      await _press(tester, LogicalKeyboardKey.select);
      await tester.pumpAndSettle();
      expect(copied(), '127.0.0.1:5353', reason: 'select on the second Copy address copies it');
    });

    // The set-port action is a long press on a tile today, so Select cannot reach it. The design says every
    // screen is operable with the remote, so this pins Select on the tile, and the port it saves.
    testWidgets(
      'setting a port by hand is reached with the remote: select on a tile asks for its port, and Save keeps it',
      (tester) async {
        _tallSurface(tester);
        final h = await _started();
        await _pumpHostScreen(tester, h);
        h.host.deliver(encode(ServicesEvent(host: _hostName, list: _services, ports: _ports)));
        await tester.pumpAndSettle();

        await _navigateUntil(tester, () => _onTile('ssh'), 'the ssh tile');
        await _press(tester, LogicalKeyboardKey.select);
        await tester.pumpAndSettle();
        expect(
          find.text('Local port for ssh'),
          findsOneWidget,
          reason: 'select on the tile asks for its local port, as a long press does',
        );

        await tester.enterText(find.byType(TextField), '2233');
        await tester.pump();
        await _navigateUntil(
          tester,
          () => _focusWithin(find.widgetWithText(TextButton, 'Save')),
          'Save in the port dialog',
          keys: const [LogicalKeyboardKey.arrowDown, LogicalKeyboardKey.arrowRight],
        );
        await _press(tester, LogicalKeyboardKey.select);
        await tester.pumpAndSettle();

        expect((await h.store.ports(h.hostId))['ssh'], 2233);
      },
    );
  });

  group('Back', () {
    // Test case 3: Back from any screen returns to the previous one. One test per screen.
    final screens = <String, Widget Function(_Harness h)>{
      'the add-host screen': (h) => AddHostScreen(
        controller: h.controller,
        store: h.store,
        hostScreenBuilder: (context, hostId) => const SizedBox(),
      ),
      'the host screen': _hostScreen,
      'the settings screen': (h) => _settings(h),
      'the diagnostics screen': (h) => _diagnostics(h),
    };
    for (final entry in screens.entries) {
      testWidgets('Back from ${entry.key} returns to the screen it was opened from', (
        tester,
      ) async {
        final h = await _started();
        await _pump(tester, _rootPage());
        final screen = entry.value(h);

        await _push(tester, screen);
        expect(find.byWidget(screen), findsOneWidget, reason: 'the screen is shown');
        await _back(tester);

        expect(find.byWidget(screen), findsNothing, reason: 'Back closes the screen');
        expect(find.text(_rootText), findsOneWidget, reason: 'Back returns to the previous screen');
      });
    }

    testWidgets('Back walks back through a stack of screens one at a time', (tester) async {
      final h = await _started();
      await _pump(tester, _rootPage());
      final settings = _settings(h);
      final diagnostics = _diagnostics(h);
      await _push(tester, settings);
      await _push(tester, diagnostics);
      expect(find.byWidget(diagnostics), findsOneWidget, reason: 'the diagnostics screen is shown');

      await _back(tester);
      expect(find.byWidget(diagnostics), findsNothing);
      expect(
        find.byWidget(settings),
        findsOneWidget,
        reason: 'Back returns to the settings screen',
      );

      await _back(tester);
      expect(find.byWidget(settings), findsNothing);
      expect(find.text(_rootText), findsOneWidget, reason: 'Back returns to the root page');
    });
  });

  group('KeyEntryField', () {
    // Test case 4: the remote reaches the field with arrow-down, and the field takes the
    // keyboard's characters. The fourth symbol moves to the next group, after a dash.
    testWidgets(
      'takes the remote keyboard characters, and the fourth symbol moves to the next group',
      (tester) async {
        await _pump(
          tester,
          _app(
            Scaffold(
              body: Column(
                children: [
                  TextButton(autofocus: true, onPressed: () {}, child: const Text('Above')),
                  const KeyEntryField(),
                ],
              ),
            ),
          ),
        );
        await tester.pumpAndSettle();
        await _press(tester, LogicalKeyboardKey.arrowDown);
        expect(
          _focusWithin(find.byType(KeyEntryField)),
          isTrue,
          reason: 'the remote reaches the key',
        );

        // The remote's keyboard types through the text input, as the on-screen keyboard does.
        await tester.enterText(find.byType(TextField), '7kq');
        await tester.pump();
        expect(_fieldText(tester), '7KQ', reason: 'three symbols are one full group');

        await tester.enterText(find.byType(TextField), '7kqm');
        await tester.pump();
        expect(_fieldText(tester), '7KQ-M', reason: 'the next symbol is typed into the next group');
        expect(_caret(tester), 5, reason: 'the caret follows the symbol');

        await tester.enterText(find.byType(TextField), '7kqm4x9tr');
        await tester.pump();
        expect(
          _fieldText(tester),
          '7KQ-M4X-9TR',
          reason: 'all nine symbols, in three groups of three',
        );
      },
    );
  });

  group('the remaining screens', () {
    testWidgets(
      'the Diagnostics row is reached with the remote, and select opens the diagnostics',
      (tester) async {
        _tallSurface(tester);
        final h = await _started();
        var opened = 0;
        await _pump(
          tester,
          _app(
            _settings(
              h,
              onOpenDiagnostics: () {
                opened++;
              },
            ),
          ),
        );
        await tester.pumpAndSettle();

        await _navigateUntil(
          tester,
          () => _focusWithin(find.byKey(_diagnosticsRowKey)),
          'the Diagnostics row',
          maxPresses: 10,
        );
        await _press(tester, LogicalKeyboardKey.select);

        expect(opened, 1, reason: 'select on the Diagnostics row opens the diagnostics screen');
      },
    );

    testWidgets('the diagnostics screen opens with focus on Copy diagnostics, its primary action', (
      tester,
    ) async {
      final h = await _started();

      await _pump(tester, _app(_diagnostics(h)));
      await tester.pumpAndSettle();

      expect(
        _focusWithin(find.widgetWithText(FilledButton, 'Copy diagnostics')),
        isTrue,
        reason: 'the first focus lands on Copy diagnostics',
      );
    });
  });
}
