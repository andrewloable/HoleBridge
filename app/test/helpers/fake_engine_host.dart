import 'dart:async';
import 'dart:typed_data';

import 'package:flutter_pear_bare/flutter_pear_bare.dart' show WorkletCrash;
import 'package:holebridge/src/engine/engine_host.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';

/// Stands in for EngineHost with no worklet behind it. It keeps EngineHost's contract, as the
/// FakeEngineHost in test/engine/engine_client_test.dart does: frames is the current worklet's
/// stream and throws before the first start, stop closes that stream, and the next start opens a
/// new one.
///
/// The frames the controller sends are kept in [sent] and decoded by [requests]. The test delivers
/// the engine's frames with [deliver]. When [onRequest] is set, it runs for each request as it is
/// sent, so a test can answer the request the way the engine would.
class FakeEngineHost extends EngineHost {
  /// The start and stop calls, in order.
  final List<String> calls = [];

  /// Every frame sent to the engine, as it was sent.
  final List<Uint8List> sent = [];

  /// Runs for each request as it is sent. Null when the test answers by hand.
  void Function(IpcRequest request)? onRequest;

  StreamController<Uint8List>? _frames;

  StreamController<WorkletCrash>? _crashes;

  /// When true, the next start() records its call, clears this flag and throws StateError('start
  /// failed') without opening a worklet.
  bool failNextStart = false;

  /// When set, stop() waits for this completer after it records its call, so a test can hold a restart
  /// in flight. Null by default.
  Completer<void>? stopGate;

  /// The requests sent so far, decoded.
  List<IpcRequest> get requests => [for (final frame in sent) decode(frame) as IpcRequest];

  /// The requests of type [T] sent so far, in the order they were sent.
  Iterable<T> requestsOf<T extends IpcRequest>() => requests.whereType<T>();

  @override
  Stream<Uint8List> get frames {
    final frames = _frames;
    if (frames == null) throw StateError('the engine is not started');
    return frames.stream;
  }

  /// The current worklet's crash reports, as frames is for its frames: throws before the first start,
  /// and its stream closes at stop.
  @override
  Stream<WorkletCrash> get crashes {
    final crashes = _crashes;
    if (crashes == null) throw StateError('the engine is not started');
    return crashes.stream;
  }

  @override
  Future<void> start() async {
    calls.add('start');
    if (failNextStart) {
      failNextStart = false;
      throw StateError('start failed');
    }
    _frames ??= StreamController<Uint8List>.broadcast();
    _crashes ??= StreamController<WorkletCrash>.broadcast();
  }

  @override
  Future<void> stop() async {
    calls.add('stop');
    final gate = stopGate;
    if (gate != null) await gate.future;
    final frames = _frames;
    _frames = null;
    final crashes = _crashes;
    _crashes = null;
    if (frames != null) await frames.close();
    if (crashes != null) await crashes.close();
  }

  /// Records the suspend, as the app going to the background does. The engine keeps its frames stream.
  @override
  Future<void> suspend() async {
    calls.add('suspend');
  }

  /// Records the resume, as the app returning to the foreground does.
  @override
  Future<void> resume() async {
    calls.add('resume');
  }

  @override
  void send(Uint8List frame) {
    sent.add(frame);
    onRequest?.call(decode(frame) as IpcRequest);
  }

  /// Delivers [frame] on the current worklet's stream, as the worklet's output would arrive.
  void deliver(Uint8List frame) => _frames!.add(frame);

  /// Reports that the worklet exited on its own: emits [c], or reason 'exit' with no detail, on the
  /// current worklet's crash stream. Does nothing when no worklet runs.
  void crash([WorkletCrash? c]) {
    _crashes?.add(c ?? (reason: 'exit', detail: null));
  }

  /// Closes the current stream, if there is one. Not awaited: a test may have no listener.
  void close() {
    final frames = _frames;
    _frames = null;
    if (frames != null) unawaited(frames.close());
  }
}
