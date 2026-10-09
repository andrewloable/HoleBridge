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

  /// Whether the next start reattaches to the worklet still running, as after a Dart hot restart.
  bool nextReattached = false;
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
            final reattached = nextReattached;
            nextReattached = false;
            return {'generationId': live, 'reattached': reattached};
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

/// Leaves a bundle copy in the temp folder, named as a copy written by the process [ownerPid].
Directory _makeCopy(int ownerPid) {
  final dir = Directory.systemTemp.createTempSync('holebridge-engine-$ownerPid-');
  File('${dir.path}${Platform.pathSeparator}engine.bundle').writeAsBytesSync(_bundle);
  return dir;
}

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
        // The bundle, and an empty addon list: these tests load no native addons.
        if (key == addonsList) return ByteData(0);
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

  test('start writes each native addon at the path the bundle loads it from', () async {
    final addon = Uint8List.fromList([5, 6, 7]);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger.setMockMessageHandler(
      'flutter/assets',
      (message) async {
        final key = utf8.decode(
          message!.buffer.asUint8List(message.offsetInBytes, message.lengthInBytes),
        );
        if (key == addonsList) {
          return ByteData.sublistView(
            Uint8List.fromList(utf8.encode('node_modules/bare-x/prebuilds/p/bare-x.bare\n')),
          );
        }
        if (key == '${addonsDir}bare-x.bare') return ByteData.sublistView(addon);
        return key == engineAsset ? ByteData.sublistView(_bundle) : null;
      },
    );
    final host = EngineHost();

    await host.start();

    final copy = _dirOf(control.bundlePaths.single);
    expect(
      File('${copy.path}/node_modules/bare-x/prebuilds/p/bare-x.bare').readAsBytesSync(),
      addon,
    );
    await host.stop();
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

  test('stop during an in-flight start terminates the worklet that start brings up', () async {
    final host = EngineHost();
    final starting = host.start();
    final stopping = host.stop();

    await Future.wait([starting, stopping]);

    expect(control.live, isNull, reason: 'a worklet survived stop');
    expect(control.calls.last, 'terminate');
    expect(
      _dirOf(control.bundlePaths.single).existsSync(),
      isFalse,
      reason: 'stop must remove the copy of the start it waited for',
    );
  });

  test('start removes the copy of a process that has exited, and keeps its own', () async {
    final gone = _makeCopy(999999999); // above any pid the OS hands out: no such process
    final host = EngineHost();

    await host.start();

    expect(gone.existsSync(), isFalse, reason: 'the copy of an exited process is still on disk');
    expect(_dirOf(control.bundlePaths.single).existsSync(), isTrue);
    await host.stop();
  });

  test('start keeps the copy of another live app process', () async {
    final other = await Process.start('sleep', ['60']);
    addTearDown(() async {
      other.kill();
      await other.exitCode;
    });
    final theirs = _makeCopy(other.pid);
    final host = EngineHost();

    await host.start();

    expect(theirs.existsSync(), isTrue, reason: 'a live process still owns this copy');
    await host.stop();
    theirs.deleteSync(recursive: true);
  });

  test(
    'a hot restart keeps the copy a reattached worklet may run from, until a fresh boot',
    () async {
      // The copy the worklet started before the hot restart; the Dart host that wrote it is gone.
      final orphan = _makeCopy(pid);
      control.nextReattached = true;
      final restarted = EngineHost();

      await restarted.start();

      expect(orphan.existsSync(), isTrue, reason: 'the reattached worklet may still run from it');
      await restarted.stop();

      final fresh = EngineHost();
      await fresh.start();

      expect(
        orphan.existsSync(),
        isFalse,
        reason: 'a fresh boot leaves no copy of this process unused',
      );
      await fresh.stop();
    },
  );
}
