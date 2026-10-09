import 'dart:async';

import 'package:holebridge/src/engine/engine_client.dart';
import 'package:holebridge/src/engine/engine_host.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';

import 'fake_engine_host.dart';

/// Stands in for EngineClient in widget tests. It records every request, answers it with the reply
/// [onRequest] builds (ok, when none is set), and passes on the events a test pushes with [push].
///
/// It implements EngineClient rather than extending it: EngineClient listens to its host's frames when
/// it is made, and a widget test has no started worklet to listen to.
class FakeEngineClient implements EngineClient {
  FakeEngineClient() : host = FakeEngineHost();

  @override
  final EngineHost host;

  final _events = StreamController<IpcEvent>.broadcast();

  /// The requests sent so far, in the order they were sent.
  final List<IpcRequest> requests = [];

  /// Builds the reply to each request. Null means every request is answered ok.
  IpcReply Function(IpcRequest request)? onRequest;

  @override
  Stream<IpcEvent> get events => _events.stream;

  /// The requests of type [T] sent so far, in the order they were sent.
  Iterable<T> requestsOf<T extends IpcRequest>() => requests.whereType<T>();

  /// Passes [event] to every listener of [events], as the engine's output would arrive.
  void push(IpcEvent event) => _events.add(event);

  /// Closes the events stream. A test calls it when it ends.
  Future<void> close() => _events.close();

  @override
  Future<IpcReply> request(IpcRequest r, {Duration timeout = const Duration(seconds: 60)}) async {
    requests.add(r);
    return onRequest?.call(r) ?? IpcReply(id: r.id, ok: true);
  }
}
