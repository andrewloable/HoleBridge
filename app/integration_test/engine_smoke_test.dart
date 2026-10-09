import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/engine/engine_host.dart';
import 'package:integration_test/integration_test.dart';

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

    expect(exits, isEmpty, reason: 'the engine exited after starting: $exits');

    await host.stop();
    await sub.cancel();
  });

  test('a frame sent to the engine comes back', () async {
    final host = EngineHost();
    await host.start();

    final echo = host.frames.first.timeout(const Duration(seconds: 10));
    host.send(Uint8List.fromList([1, 2, 3]));
    expect(await echo, [1, 2, 3]);

    await host.stop();
  });
}
