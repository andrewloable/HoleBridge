import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/engine/engine_host.dart';

// The engine bundle the mocked asset channel returns. Its content does not matter here.
final _bundle = Uint8List.fromList(List.filled(64, 7));

/// Stands in for the native side of flutter_pear_bare's control channel. start hands out a new
/// generation id each time, and a test can make the next start or the next terminate fail.
/// exitNative sends onWorkletExit the way the native plugin does when the worklet dies.
class FakeControl {
  final List<String> bundlePaths = [];
  final List<String> calls = [];
  PlatformException? nextStartError;
  PlatformException? terminateError;
  int _generation = 0;

  /// The generation of the worklet the fake native side still runs, or null when none does.
  /// BareWorklet is a process-wide singleton, so a test must leave none running.
  int? live;

  void install() {
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger.setMockMethodCallHandler(
      const MethodChannel('flutter_pear_bare/control'),
      (call) async {
        calls.add(call.method);
        switch (call.method) {
          case 'start':
            bundlePaths.add((call.arguments as Map)['bundlePath'] as String);
            final error = nextStartError;
            if (error != null) {
              nextStartError = null;
              throw error;
            }
            live = ++_generation;
            return {'generationId': live, 'reattached': false};
          case 'terminate':
            final error = terminateError;
            if (error != null) throw error;
            live = null;
            return null;
        }
        return null;
      },
    );
  }

  void uninstall() {
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger.setMockMethodCallHandler(
      const MethodChannel('flutter_pear_bare/control'),
      null,
    );
  }

  Future<void> exitNative(int generation) {
    if (live == generation) live = null;
    final data = const StandardMethodCodec().encodeMethodCall(
      MethodCall('onWorkletExit', {'reason': 'test-exit', 'generationId': generation}),
    );
    return TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger.handlePlatformMessage(
      'flutter_pear_bare/control',
      data,
      (_) {},
    );
  }
}

/// The directory that holds a bundle copy, which EngineHost must create and remove.
Directory _dirOf(String bundlePath) => File(bundlePath).parent;

/// The bundle copy directories currently in the system temp folder.
Set<String> _engineDirs() => Directory.systemTemp
    .listSync()
    .whereType<Directory>()
    .map((d) => d.path)
    .where((path) => path.split(Platform.pathSeparator).last.startsWith('holebridge-engine-'))
    .toSet();

void main() {
  late FakeControl control;
  late Set<String> engineDirsBefore;

  setUp(() {
    TestWidgetsFlutterBinding.ensureInitialized();
    engineDirsBefore = _engineDirs();
    control = FakeControl()..install();
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger.setMockMessageHandler(
      'flutter/assets',
      (message) async {
        final key = utf8.decode(
          message!.buffer.asUint8List(message.offsetInBytes, message.lengthInBytes),
        );
        return key == engineAsset ? ByteData.sublistView(_bundle) : null;
      },
    );
  });

  tearDown(() async {
    // A test that failed midway may leave the singleton worklet running; the native exit resets it.
    final live = control.live;
    if (live != null) await control.exitNative(live);
    control.uninstall();
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger.setMockMessageHandler(
      'flutter/assets',
      null,
    );
    // Whatever the test did, no bundle copy it made may be left in the temp folder.
    expect(
      _engineDirs().difference(engineDirsBefore),
      isEmpty,
      reason: 'a bundle copy was left in the system temp folder',
    );
  });

  test('a restart after the worklet exits removes the first bundle copy', () async {
    final host = EngineHost();
    await host.start();
    await control.exitNative(1);

    await host.start();

    expect(control.bundlePaths, hasLength(2));
    expect(
      _dirOf(control.bundlePaths.first).existsSync(),
      isFalse,
      reason: 'the copy of the exited worklet is still on disk',
    );
    await host.stop();
    expect(_dirOf(control.bundlePaths.last).existsSync(), isFalse);
  });

  test('a failed start removes its bundle copy, and a retry starts cleanly', () async {
    control.nextStartError = PlatformException(code: 'bare_runtime_missing');
    final host = EngineHost();

    await expectLater(host.start(), throwsA(isA<PlatformException>()));
    expect(
      _dirOf(control.bundlePaths.single).existsSync(),
      isFalse,
      reason: 'the copy from the failed start is still on disk',
    );

    await host.start();
    expect(control.bundlePaths, hasLength(2));
    await host.stop();
    expect(_dirOf(control.bundlePaths.last).existsSync(), isFalse);
  });

  test('overlapping starts share one native start and one bundle copy', () async {
    final host = EngineHost();

    await Future.wait([host.start(), host.start()]);

    expect(control.calls.where((c) => c == 'start'), hasLength(1));
    expect(control.bundlePaths, hasLength(1));
    await host.stop();
    expect(_dirOf(control.bundlePaths.single).existsSync(), isFalse);
  });

  test('stop removes the bundle copy even when terminate throws', () async {
    final host = EngineHost();
    await host.start();
    control.terminateError = PlatformException(code: 'terminate_failed');

    await expectLater(host.stop(), throwsA(isA<PlatformException>()));

    expect(
      _dirOf(control.bundlePaths.single).existsSync(),
      isFalse,
      reason: 'stop must remove the copy even though terminate failed',
    );
    expect(() => host.frames, throwsStateError, reason: 'stop must forget the worklet');
  });
}
