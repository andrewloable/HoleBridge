import 'dart:async';

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
import 'package:holebridge/src/ui/home_screen.dart';
import 'package:holebridge/src/ui/key_entry_field.dart';

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

/// An AppController that records each addTypedKey call and does not connect. The call is the thing
/// under test here, so the engine is not asked.
class _RecordingController extends AppController {
  _RecordingController(super.host, super.store);

  final List<String> typedKeys = [];

  @override
  Future<String?> addTypedKey(String typed) async {
    typedKeys.add(typed);
    return null;
  }
}

// Test values only (docs/security.md): an application key of 32 bytes, and 9-symbol keys in the
// Crockford alphabet. The typed key has lower case and dashes, and normalizes to _key.
final _appKey = Uint8List.fromList(List<int>.generate(32, (i) => i));
const _key = '7KQM4X9TR';
const _otherKey = 'HJKMNPQRS';
const _typedKey = '7kq-m4x-9tr';
final _validLink = buildKeyLink('https://holebridge.app', _key, _appKey);
// A key link with no application key after the dot.
final _invalidLink = 'https://holebridge.app/k#$_key';

const _hostScreenKey = Key('host-screen-stub');
const _fakeScanKey = Key('fake-scanner-read');

/// Stands in for the host screen, which is another screen's task.
Widget _hostScreenStub(BuildContext context, String hostId) =>
    const Center(key: _hostScreenKey, child: Text('Host screen'));

/// Stands in for the camera. Tapping it reads [_validLink], as a camera would read a QR code.
Widget _fakeScanner(BuildContext context, void Function(String code) onCode) => Center(
  child: TextButton(
    key: _fakeScanKey,
    onPressed: () => onCode(_validLink),
    child: const Text('Read code'),
  ),
);

/// An engine that answers every request ok, as the engine does for a connect that finds its host.
FakeEngineHost _answeringEngine() {
  final host = FakeEngineHost();
  host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));
  addTearDown(host.close);
  return host;
}

/// Starts a controller over [host] and [store]. The controller is disposed when the test ends.
Future<AppController> _started(FakeEngineHost host, HostStore store) async {
  final controller = AppController(host, store);
  addTearDown(controller.dispose);
  await controller.start();
  return controller;
}

/// The page under test in a MaterialApp, so it has a Navigator.
Widget _app(Widget home) => MaterialApp(home: home);

/// Sets what the clipboard holds. Paste link reads it through the platform channel.
void _clipboard(WidgetTester tester, String text) {
  tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (
    call,
  ) async {
    if (call.method == 'Clipboard.getData') return <String, dynamic>{'text': text};
    return null;
  });
  addTearDown(
    () => tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(
      SystemChannels.platform,
      null,
    ),
  );
}

/// Lets frames, animations and the engine's replies run.
Future<void> _settle(WidgetTester tester) async {
  await tester.pump();
  await tester.pump(const Duration(seconds: 1));
}

/// Every text the page shows, joined and lowercased, so a check does not depend on capitals.
String _shownText(WidgetTester tester) {
  final parts = [
    for (final widget in tester.widgetList<Text>(find.byType(Text)))
      widget.data ?? widget.textSpan?.toPlainText() ?? '',
  ];
  return parts.join('\n').toLowerCase();
}

/// Types [text] into the key field and presses Add host.
Future<void> _typeKeyAndAdd(WidgetTester tester, String text) async {
  await tester.enterText(find.byType(TextField), text);
  await tester.tap(find.text('Add host'));
  await _settle(tester);
}

void main() {
  testWidgets('with no hosts, the app starts on AddHostScreen', (tester) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);

    await tester.pumpWidget(
      _app(HomeScreen(controller: controller, store: store, hostScreenBuilder: _hostScreenStub)),
    );
    await _settle(tester);

    expect(find.byType(AddHostScreen), findsOneWidget);
  });

  testWidgets('pasting a valid key link adds the host and navigates to the host screen', (
    tester,
  ) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);
    _clipboard(tester, _validLink);

    await tester.pumpWidget(
      _app(AddHostScreen(controller: controller, store: store, hostScreenBuilder: _hostScreenStub)),
    );
    await tester.tap(find.text('Paste link'));
    await _settle(tester);

    expect(find.byKey(_hostScreenKey), findsOneWidget);
    expect([for (final stored in await store.hosts()) stored.key], [_key]);
    expect(await store.appKeys(), [_appKey]);
    final connect = host.requestsOf<ConnectRequest>().single;
    expect(connect.key, _key);
    expect(connect.appKey, _appKey);
  });

  testWidgets('pasting an invalid link shows an error and adds nothing', (tester) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);
    _clipboard(tester, _invalidLink);

    await tester.pumpWidget(
      _app(AddHostScreen(controller: controller, store: store, hostScreenBuilder: _hostScreenStub)),
    );
    await tester.tap(find.text('Paste link'));
    await _settle(tester);

    expect(find.textContaining('HB-KEY-INVALID'), findsWidgets);
    expect(find.byKey(_hostScreenKey), findsNothing);
    expect(await store.hosts(), isEmpty);
    expect(host.requestsOf<ConnectRequest>(), isEmpty);
  });

  testWidgets('typing 7kq-m4x-9tr with a held app key calls addTypedKey with it', (tester) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    // Adding a host holds its application key, so a typed key has one to be tried with.
    await store.addHost(name: 'Home', key: _otherKey, appKey: _appKey);
    final controller = _RecordingController(host, store);
    addTearDown(controller.dispose);
    await controller.start();

    await tester.pumpWidget(
      _app(AddHostScreen(controller: controller, store: store, hostScreenBuilder: _hostScreenStub)),
    );
    await _typeKeyAndAdd(tester, _typedKey);

    expect(controller.typedKeys, hasLength(1));
    expect(normalizeKey(controller.typedKeys.single), _key);
  });

  testWidgets('typing a key with no held app key shows the explanation text', (tester) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);

    await tester.pumpWidget(
      _app(AddHostScreen(controller: controller, store: store, hostScreenBuilder: _hostScreenStub)),
    );
    await _typeKeyAndAdd(tester, _typedKey);

    final text = _shownText(tester);
    expect(text, contains('qr code'));
    expect(text, isNot(contains('hb-key-invalid')), reason: 'the key itself is valid');
    expect(host.requestsOf<ConnectRequest>(), isEmpty, reason: 'no application key, no dial');
    expect(await store.hosts(), isEmpty);
  });

  testWidgets('a scanned QR code with a key link adds the host and navigates to the host screen', (
    tester,
  ) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);

    await tester.pumpWidget(
      _app(
        AddHostScreen(
          controller: controller,
          store: store,
          hostScreenBuilder: _hostScreenStub,
          scannerBuilder: _fakeScanner,
        ),
      ),
    );
    await tester.tap(find.text('Scan QR'));
    await _settle(tester);
    await tester.tap(find.byKey(_fakeScanKey));
    await _settle(tester);

    expect(find.byKey(_hostScreenKey), findsOneWidget);
    expect([for (final stored in await store.hosts()) stored.key], [_key]);
    final connect = host.requestsOf<ConnectRequest>().single;
    expect(connect.key, _key);
    expect(connect.appKey, _appKey);
  });

  testWidgets('on a TV, Add from phone is shown and Scan QR is not', (tester) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);

    await tester.pumpWidget(
      _app(
        AddHostScreen(
          controller: controller,
          store: store,
          hostScreenBuilder: _hostScreenStub,
          // A camera is offered, so Scan QR is hidden because of the TV and not because of the camera.
          scannerBuilder: _fakeScanner,
          isTv: true,
          onAddFromPhone: () {},
        ),
      ),
    );
    await _settle(tester);

    expect(find.text('Add from phone'), findsOneWidget);
    expect(find.text('Scan QR'), findsNothing);
  });

  // Edge cases beyond the seven above.

  testWidgets('repeated codes from a live camera after the first valid one are ignored', (
    tester,
  ) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);
    // The camera's onCode, kept so the test can call it again as a live camera does.
    void Function(String code)? readCode;
    Widget camera(BuildContext context, void Function(String code) onCode) {
      readCode = onCode;
      return const SizedBox.shrink();
    }

    await tester.pumpWidget(
      _app(
        AddHostScreen(
          controller: controller,
          store: store,
          hostScreenBuilder: _hostScreenStub,
          scannerBuilder: camera,
        ),
      ),
    );
    await tester.tap(find.text('Scan QR'));
    await _settle(tester);
    readCode!(_validLink);
    readCode!(_validLink);
    await _settle(tester);

    expect(host.requestsOf<ConnectRequest>(), hasLength(1));
    expect(await store.hosts(), hasLength(1));
  });

  testWidgets('with a stored host, the app opens on the first host screen', (tester) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    await store.addHost(name: 'Home', key: _key, appKey: _appKey);
    final controller = await _started(host, store);
    String? opened;
    Widget openHost(BuildContext context, String hostId) {
      opened = hostId;
      return _hostScreenStub(context, hostId);
    }

    await tester.pumpWidget(
      _app(HomeScreen(controller: controller, store: store, hostScreenBuilder: openHost)),
    );
    await _settle(tester);

    expect(find.byType(AddHostScreen), findsNothing);
    expect(find.byKey(_hostScreenKey), findsOneWidget);
    expect(opened, (await store.hosts()).single.id);
  });

  testWidgets('the key field writes a typed key as three groups of three', (tester) async {
    await tester.pumpWidget(_app(const Scaffold(body: KeyEntryField())));

    await tester.enterText(find.byType(TextField), '7kqm4x9tr');

    expect(tester.widget<EditableText>(find.byType(EditableText)).controller.text, '7KQ-M4X-9TR');
  });

  testWidgets('the key field keeps a character outside the BMP whole after two symbols', (
    tester,
  ) async {
    await tester.pumpWidget(_app(const Scaffold(body: KeyEntryField())));

    await tester.enterText(find.byType(TextField), 'AB\u{1F600}');
    await tester.pump();

    expect(tester.takeException(), isNull);
    expect(_fieldText(tester), 'AB\u{1F600}');
  });

  testWidgets('the key field does not split a character outside the BMP at the ninth symbol', (
    tester,
  ) async {
    await tester.pumpWidget(_app(const Scaffold(body: KeyEntryField())));

    await tester.enterText(find.byType(TextField), 'ABCDEFGH\u{1F600}');
    await tester.pump();

    expect(tester.takeException(), isNull);
    expect(_fieldText(tester), 'ABC-DEF-GH\u{1F600}');
  });

  testWidgets('the key field keeps a character outside the BMP whole after a dash', (
    tester,
  ) async {
    await tester.pumpWidget(_app(const Scaffold(body: KeyEntryField())));

    await tester.enterText(find.byType(TextField), '7kq\u{1F600}');
    await tester.pump();

    expect(tester.takeException(), isNull);
    expect(_fieldText(tester), '7KQ-\u{1F600}');
  });

  for (final failure in <Object>[StateError('x'), TimeoutException('x'), const IpcDesync()]) {
    testWidgets('an engine failure from connect (${failure.runtimeType}) shows the engine notice', (
      tester,
    ) async {
      final host = _answeringEngine();
      final store = HostStore(_MemoryBackend());
      final controller = _ThrowingController(host, store, failure);
      addTearDown(controller.dispose);
      await controller.start();
      _clipboard(tester, _validLink);

      await tester.pumpWidget(
        _app(
          AddHostScreen(controller: controller, store: store, hostScreenBuilder: _hostScreenStub),
        ),
      );
      await tester.tap(find.text('Paste link'));
      await _settle(tester);

      expect(find.textContaining('did not answer'), findsOneWidget);
      expect(find.byKey(_hostScreenKey), findsNothing);
      expect(tester.takeException(), isNull);
    });
  }

  testWidgets('a programming error from connect is not shown as an engine notice', (tester) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = _ThrowingController(host, store, ArgumentError('bug'));
    addTearDown(controller.dispose);
    await controller.start();
    _clipboard(tester, _validLink);

    await tester.pumpWidget(
      _app(AddHostScreen(controller: controller, store: store, hostScreenBuilder: _hostScreenStub)),
    );
    final errors = <Object>[];
    await runZonedGuarded(
      () async {
        await tester.tap(find.text('Paste link'));
        await _settle(tester);
      },
      (error, stack) {
        errors.add(error);
      },
    );

    expect(find.textContaining('did not answer'), findsNothing);
    expect(errors, [isA<ArgumentError>()]);
    expect(
      tester.widget<OutlinedButton>(find.widgetWithText(OutlinedButton, 'Paste link')).onPressed,
      isNotNull,
      reason: 'the screen is usable again',
    );
  });

  testWidgets('a pasted link with a cut-short application key shows HB-APPKEY-INVALID', (
    tester,
  ) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);
    _clipboard(tester, 'https://holebridge.app/k#$_key.${'a' * 63}');

    await tester.pumpWidget(
      _app(AddHostScreen(controller: controller, store: store, hostScreenBuilder: _hostScreenStub)),
    );
    await tester.tap(find.text('Paste link'));
    await _settle(tester);

    expect(find.textContaining('HB-APPKEY-INVALID'), findsOneWidget);
    expect(find.textContaining('HB-KEY-INVALID'), findsNothing);
    final text = _shownText(tester);
    expect(text, isNot(contains('holebridge.app')), reason: 'the link is never shown');
    expect(text, isNot(contains(_key.toLowerCase())), reason: 'the key is never shown');
    expect(await store.hosts(), isEmpty);
    expect(host.requestsOf<ConnectRequest>(), isEmpty);
  });

  testWidgets('Scan QR is disabled while an add runs and enabled again when it ends', (
    tester,
  ) async {
    // The engine never answers, so the add waits until its connect times out.
    final host = FakeEngineHost();
    addTearDown(host.close);
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);

    await tester.pumpWidget(
      _app(
        AddHostScreen(
          controller: controller,
          store: store,
          hostScreenBuilder: _hostScreenStub,
          scannerBuilder: _fakeScanner,
        ),
      ),
    );
    await tester.tap(find.text('Scan QR'));
    await _settle(tester);
    await tester.tap(find.byKey(_fakeScanKey));
    await _settle(tester);

    expect(
      tester.widget<FilledButton>(find.widgetWithText(FilledButton, 'Scan QR')).onPressed,
      isNull,
    );
    await tester.pump(const Duration(seconds: 80));
    expect(find.textContaining('did not answer'), findsOneWidget);
    expect(
      tester.widget<FilledButton>(find.widgetWithText(FilledButton, 'Scan QR')).onPressed,
      isNotNull,
    );
  });

  testWidgets('a second, different valid code from a live camera after the first is ignored', (
    tester,
  ) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);
    void Function(String code)? readCode;
    Widget camera(BuildContext context, void Function(String code) onCode) {
      readCode = onCode;
      return const SizedBox.shrink();
    }

    await tester.pumpWidget(
      _app(
        AddHostScreen(
          controller: controller,
          store: store,
          hostScreenBuilder: _hostScreenStub,
          scannerBuilder: camera,
        ),
      ),
    );
    await tester.tap(find.text('Scan QR'));
    await _settle(tester);
    readCode!(_validLink);
    readCode!(buildKeyLink('https://holebridge.app', _otherKey, _appKey));
    await _settle(tester);

    expect(host.requestsOf<ConnectRequest>(), hasLength(1));
    expect(await store.hosts(), hasLength(1));
  });

  testWidgets('the same valid code read twice from a live camera shows no notice', (tester) async {
    final host = _answeringEngine();
    final store = HostStore(_MemoryBackend());
    final controller = await _started(host, store);
    void Function(String code)? readCode;
    Widget camera(BuildContext context, void Function(String code) onCode) {
      readCode = onCode;
      return const SizedBox.shrink();
    }

    await tester.pumpWidget(
      _app(
        AddHostScreen(
          controller: controller,
          store: store,
          hostScreenBuilder: _hostScreenStub,
          scannerBuilder: camera,
        ),
      ),
    );
    await tester.tap(find.text('Scan QR'));
    await _settle(tester);
    readCode!(_validLink);
    readCode!(_validLink);
    await _settle(tester);

    expect(find.textContaining('did not answer', skipOffstage: false), findsNothing);
  });
}

/// An AppController whose connect throws [error], as an engine failure or a bug would.
class _ThrowingController extends AppController {
  _ThrowingController(super.host, super.store, this.error);

  final Object error;

  @override
  Future<void> connect(String hostId) async => throw error;
}

/// The text the key field holds.
String _fieldText(WidgetTester tester) =>
    tester.widget<EditableText>(find.byType(EditableText)).controller.text;
