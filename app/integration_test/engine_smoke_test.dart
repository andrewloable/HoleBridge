import 'dart:io';
import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/engine/engine_host.dart';
import 'package:integration_test/integration_test.dart';

// Frames reach the engine only through BareKit.IPC, which exists on mobile. The desktop stdin and
// stdout channel is HoleBridge-3y9.14, so the echo check is skipped on desktop until it lands.
final _desktop = !(Platform.isAndroid || Platform.isIOS);

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();

  test('the engine bundle loads and runs', () async {
    final host = EngineHost();
    await host.start();
    final exits = <String>[];
    final sub = host.crashes.listen((crash) => exits.add(crash.reason));

    await host.suspend();
    await host.resume();
    // A bundle that fails to load exits almost at once, within this window.
    await Future<void>.delayed(const Duration(seconds: 2));

    if (_desktop) {
      // Nothing reads the desktop stdin until HoleBridge-3y9.14, so bare exits once the script has
      // run. That is a clean exit (status 0), which only a bundle that loaded can give. A load
      // failure exits non-zero.
      expect(
        exits.where((reason) => !reason.endsWith('(status 0)')),
        isEmpty,
        reason: 'the engine failed to load: $exits',
      );
    } else {
      expect(exits, isEmpty, reason: 'the engine exited after starting');
    }

    await host.stop();
    await sub.cancel();
  });

  test(
    'a frame sent to the engine comes back',
    skip: _desktop,
    () async {
      final host = EngineHost();
      await host.start();

      final echo = host.frames.first.timeout(const Duration(seconds: 10));
      host.send(Uint8List.fromList([1, 2, 3]));
      expect(await echo, [1, 2, 3]);

      await host.stop();
    },
  );
}
