import 'dart:async';
import 'dart:typed_data';

import 'package:flutter_pear_bare/flutter_pear_bare.dart' show WorkletCrash;
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/engine/engine_client.dart';
import 'package:holebridge/src/engine/engine_host.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';

/// Stands in for EngineHost with no worklet behind it, and keeps EngineHost's contract: frames is
/// the current worklet's stream and throws before the first start, stop closes that stream, and
/// the next start opens a new one. start and stop record their calls, the frames the client sends
/// are kept for the test to read, and the test delivers the engine's frames with [deliver], as the
/// worklet's output would arrive.
class FakeEngineHost extends EngineHost {
  final List<String> calls = [];
  final List<Uint8List> sent = [];
  StreamController<Uint8List>? _frames;

  @override
  Stream<Uint8List> get frames {
    final frames = _frames;
    if (frames == null) throw StateError('the engine is not started');
    return frames.stream;
  }

  @override
  Stream<WorkletCrash> get crashes => const Stream<WorkletCrash>.empty();

  @override
  Future<void> start() async {
    calls.add('start');
    _frames ??= StreamController<Uint8List>.broadcast();
  }

  @override
  Future<void> stop() async {
    calls.add('stop');
    final frames = _frames;
    _frames = null;
    if (frames != null) await frames.close();
  }

  @override
  void send(Uint8List frame) {
    sent.add(frame);
  }

  /// Delivers [frame] on the current worklet's stream.
  void deliver(Uint8List frame) => _frames!.add(frame);

  /// Closes the current stream, if there is one. Not awaited: a test may have no listener.
  void close() {
    final frames = _frames;
    _frames = null;
    if (frames != null) unawaited(frames.close());
  }
}

/// Bytes from a hex string.
Uint8List _bytes(String hex) => Uint8List.fromList([
  for (var i = 0; i < hex.length; i += 2) int.parse(hex.substring(i, i + 2), radix: 16),
]);

void main() {
  test('request() completes with the reply carrying the same id', () async {
    final host = FakeEngineHost();
    addTearDown(host.close);
    await host.start();
    final client = EngineClient(host);

    final reply = client.request(StatusRequest(host: 'living-room'));
    await pumpEventQueue();

    expect(host.sent, hasLength(1));
    final request = decode(host.sent.single);
    expect(request, isA<StatusRequest>());
    expect(request.id, isNot(0), reason: 'Dart picks request ids, never 0');

    host.deliver(encode(IpcReply(id: request.id, ok: true, route: 'direct')));
    final got = await reply;
    expect(got.ok, isTrue);
    expect(got.route, 'direct');
  });

  test(
    'two concurrent requests get their own replies when the replies arrive out of order',
    () async {
      final host = FakeEngineHost();
      addTearDown(host.close);
      await host.start();
      final client = EngineClient(host);

      final first = client.request(StatusRequest(host: 'living-room'));
      final second = client.request(StatusRequest(host: 'office'));
      await pumpEventQueue();

      expect(host.sent, hasLength(2));
      final firstId = decode(host.sent[0]).id;
      final secondId = decode(host.sent[1]).id;
      expect(secondId, isNot(firstId));

      // The second request is answered first.
      host.deliver(
        encode(
          IpcReply(id: secondId, ok: false, code: 'HB-USAGE', detail: 'host is not connected'),
        ),
      );
      host.deliver(encode(IpcReply(id: firstId, ok: true, route: 'lan')));

      final firstReply = await first;
      final secondReply = await second;
      expect(firstReply.ok, isTrue);
      expect(firstReply.route, 'lan');
      expect(secondReply.ok, isFalse);
      expect(secondReply.code, 'HB-USAGE');
    },
  );

  test('an event frame appears on the events stream', () async {
    final host = FakeEngineHost();
    addTearDown(host.close);
    await host.start();
    final client = EngineClient(host);

    final event = client.events.first;
    // route 1 from spec/vectors/ipc.json: living-room is reached over the LAN.
    host.deliver(_bytes('64000b6c6976696e672d726f6f6d036c616e'));

    final got = await event;
    expect(got, isA<RouteEvent>());
    final route = got as RouteEvent;
    expect(route.host, 'living-room');
    expect(route.route, 'lan');
  });

  test(
    'a garbage frame restarts the engine, fails pending requests and emits HB-IPC-DESYNC',
    () async {
      final host = FakeEngineHost();
      addTearDown(host.close);
      await host.start();
      final client = EngineClient(host);

      final pending = client.request(StatusRequest(host: 'living-room'));
      final pendingFailure = expectLater(pending, throwsA(isA<IpcDesync>()));
      final desyncEvent = client.events.firstWhere((e) => e is ErrorEvent);
      await pumpEventQueue();

      // A route event whose host string claims 11 bytes and has none: the frame does not decode.
      host.deliver(Uint8List.fromList([0x64, 0x00, 0x0b]));

      await pendingFailure;
      final event = await desyncEvent;
      expect(event, isA<ErrorEvent>());
      expect((event as ErrorEvent).code, 'HB-IPC-DESYNC');

      await pumpEventQueue();
      expect(host.calls, [
        'start',
        'stop',
        'start',
      ], reason: 'the engine is stopped, then started again');
    },
  );

  test('after a restart the client works with the restarted engine', () async {
    final host = FakeEngineHost();
    addTearDown(host.close);
    await host.start();
    final client = EngineClient(host);

    final pending = client.request(StatusRequest(host: 'living-room'));
    final pendingFailure = expectLater(pending, throwsA(isA<IpcDesync>()));
    await pumpEventQueue();
    host.deliver(Uint8List.fromList([0x64, 0x00, 0x0b]));
    await pendingFailure;
    await pumpEventQueue();
    expect(host.calls, ['start', 'stop', 'start']);

    // The restarted engine is a new worklet with a new frame stream.
    final event = client.events.firstWhere((e) => e is RouteEvent);
    final reply = client.request(StatusRequest(host: 'living-room'));
    await pumpEventQueue();
    expect(host.sent, hasLength(2));
    final request = decode(host.sent.last);
    host.deliver(_bytes('64000b6c6976696e672d726f6f6d036c616e'));
    host.deliver(encode(IpcReply(id: request.id, ok: true, route: 'lan')));

    expect((await reply).route, 'lan');
    expect((await event as RouteEvent).host, 'living-room');
    expect(host.calls, ['start', 'stop', 'start'], reason: 'one restart, not two');
  });

  test('EngineClient on a host that was never started throws StateError', () {
    expect(() => EngineClient(FakeEngineHost()), throwsStateError);
  });

  test('a reply for no pending request is ignored, without a restart', () async {
    final host = FakeEngineHost();
    addTearDown(host.close);
    await host.start();
    final client = EngineClient(host);

    host.deliver(encode(IpcReply(id: 99, ok: true)));
    final reply = client.request(StatusRequest(host: 'living-room'));
    await pumpEventQueue();
    final request = decode(host.sent.single);
    host.deliver(encode(IpcReply(id: request.id, ok: true, route: 'lan')));

    expect((await reply).route, 'lan');
    expect(host.calls, ['start'], reason: 'a stray reply does not restart the engine');
  });

  test('a request frame from the engine is a desync too', () async {
    final host = FakeEngineHost();
    addTearDown(host.close);
    await host.start();
    final client = EngineClient(host);

    final pending = client.request(StatusRequest(host: 'living-room'));
    final pendingFailure = expectLater(pending, throwsA(isA<IpcDesync>()));
    final desyncEvent = client.events.firstWhere((e) => e is ErrorEvent);
    await pumpEventQueue();

    // The engine never sends a request, so this well-formed frame means the stream is out of step.
    host.deliver(encode(CloseRequest(id: 7, host: 'living-room')));

    await pendingFailure;
    expect(((await desyncEvent) as ErrorEvent).code, 'HB-IPC-DESYNC');
    await pumpEventQueue();
    expect(host.calls, ['start', 'stop', 'start']);
  });

  testWidgets('request() times out with a TimeoutException after the timeout', (tester) async {
    final host = FakeEngineHost();
    addTearDown(host.close);
    await host.start();
    final client = EngineClient(host);

    Object? failure;
    unawaited(
      client
          .request(StatusRequest(host: 'living-room'), timeout: const Duration(seconds: 5))
          .then<void>(
            (_) {},
            onError: (Object e) {
              failure = e;
            },
          ),
    );

    await tester.pump(const Duration(seconds: 4));
    expect(failure, isNull, reason: 'no timeout before 5 s');
    await tester.pump(const Duration(seconds: 1));
    expect(failure, isA<TimeoutException>());
  });

  testWidgets('request() waits 60 s when no timeout is given', (tester) async {
    final host = FakeEngineHost();
    addTearDown(host.close);
    await host.start();
    final client = EngineClient(host);

    Object? failure;
    unawaited(
      client
          .request(StatusRequest(host: 'living-room'))
          .then<void>(
            (_) {},
            onError: (Object e) {
              failure = e;
            },
          ),
    );

    await tester.pump(const Duration(seconds: 59));
    expect(failure, isNull, reason: 'no timeout before 60 s');
    await tester.pump(const Duration(seconds: 1));
    expect(failure, isA<TimeoutException>());
  });
}
