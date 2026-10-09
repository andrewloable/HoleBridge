import 'dart:async';
import 'dart:typed_data';

import 'engine_host.dart';
import 'ipc_codec.dart';

/// The Dart side of the app engine IPC (spec/ipc.md). It sends requests through an [EngineHost],
/// pairs each reply with its request by id, and passes the engine's events on.
///
/// The client listens to [EngineHost.frames] from its constructor, so the host must be started
/// first: on a host that was never started the constructor throws StateError. After each restart
/// the client makes itself, it listens to the new worklet's frames again, because a worklet's
/// frame stream closes when the worklet stops, and then calls the onRestart callback it was made with.
/// A frame from the engine that is not one it may send is fatal (see [request]).
class EngineClient {
  /// [onRestart] runs after each restart the client makes, once the client listens to the new worklet's
  /// frames. The app controller listens to the new worklet's crashes there, because it cannot see the
  /// restart otherwise. Null when nothing needs to know.
  EngineClient(this.host, {void Function()? onRestart}) {
    _onRestart = onRestart;
    _subscribe();
  }

  /// The engine the frames go to and come from.
  final EngineHost host;

  void Function()? _onRestart;

  // Events go to every listener that is attached when they arrive. An event that arrives while
  // nothing listens is dropped, so attach to [events] before sending requests.
  final _events = StreamController<IpcEvent>.broadcast();
  final _pending = <int, Completer<IpcReply>>{};
  StreamSubscription<Uint8List>? _frames;

  /// The restart in progress after a desync, or null when none runs.
  Future<void>? _restart;
  int _lastId = 0;

  /// Sends [r] and completes with its reply, the one that carries the same id.
  ///
  /// The engine always replies, so a reply with ok false is a result, not an error. The future
  /// fails with a TimeoutException when no reply comes within [timeout]. A malformed frame from the
  /// engine is fatal: it fails every pending request with IpcDesync, emits an ErrorEvent with code
  /// HB-IPC-DESYNC on [events], then stops and restarts the engine.
  Future<IpcReply> request(IpcRequest r, {Duration timeout = const Duration(seconds: 60)}) async {
    final id = _nextId();
    final reply = Completer<IpcReply>();
    _pending[id] = reply;
    try {
      // A request made during a restart waits for it, because the engine is not running then.
      final restart = _restart;
      if (restart != null) await restart;
      // A desync during that wait has already failed this request.
      if (!reply.isCompleted) {
        if (_frames == null) _subscribe();
        host.send(encode(r.withId(id)));
      }
      return await reply.future.timeout(timeout);
    } finally {
      _pending.remove(id);
    }
  }

  /// The engine's events (types 100 and up), in the order they arrive.
  Stream<IpcEvent> get events => _events.stream;

  /// The next request id: 1 up to 2^31 - 1, then 1 again, skipping any id still in use.
  int _nextId() {
    do {
      _lastId = _lastId >= 0x7fffffff ? 1 : _lastId + 1;
    } while (_pending.containsKey(_lastId));
    return _lastId;
  }

  /// Listens to the current worklet's frames. Its subscription is kept until a desync cancels it,
  /// or until the stream closes on its own.
  void _subscribe() {
    late final StreamSubscription<Uint8List> subscription;
    subscription = host.frames.listen(
      _onFrame,
      onDone: () {
        if (identical(_frames, subscription)) _frames = null;
      },
    );
    _frames = subscription;
  }

  void _onFrame(Uint8List frame) {
    final IpcMessage message;
    try {
      message = decode(frame);
    } on IpcDesync {
      _onDesync();
      return;
    }
    switch (message) {
      case IpcReply():
        // A reply with no request waiting is a late one, for a request that timed out. Ignore it.
        _pending.remove(message.id)?.complete(message);
      case IpcEvent():
        _events.add(message);
      case IpcRequest():
        // The engine never sends a request, so this frame means the stream is out of step.
        _onDesync();
    }
  }

  /// Fails every pending request, emits HB-IPC-DESYNC and restarts the engine. The detail carries
  /// no frame bytes.
  void _onDesync() {
    final frames = _frames;
    _frames = null;
    unawaited(frames?.cancel());

    final failed = _pending.values.toList();
    _pending.clear();
    for (final reply in failed) {
      if (!reply.isCompleted) reply.completeError(const IpcDesync());
    }

    _events.add(
      const ErrorEvent(
        code: 'HB-IPC-DESYNC',
        detail: 'a frame from the engine did not decode; the engine is restarting',
      ),
    );
    _restart = _restartEngine();
  }

  /// Stops the engine, starts it again, listens to its frames and then calls [_onRestart]. The future
  /// never fails. If a step fails, no subscription is made, and the next request tries to subscribe
  /// again: it fails with StateError while the engine is not running.
  Future<void> _restartEngine() async {
    try {
      await host.stop();
      await host.start();
      _subscribe();
      _onRestart?.call();
    } catch (_) {
      // Nothing is reported here; requests fail until the engine runs again.
    } finally {
      _restart = null;
    }
  }
}
