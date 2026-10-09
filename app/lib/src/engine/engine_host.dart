import 'dart:async';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:flutter_pear_bare/flutter_pear_bare.dart';

/// The asset key of the engine bundle, written by tool/copy_engine_bundle.sh.
const engineAsset = 'assets/engine.bundle';

/// Runs HoleBridge's app engine (app/engine) in a Bare worklet and carries raw binary frames to it.
///
/// The frames are HoleBridge's own IPC, decoded by the layer above. This class only starts, stops
/// and pauses the worklet. It uses flutter_pear_bare's BareWorklet, not flutter_pear's Pear class.
class EngineHost {
  BareWorklet? _worklet;
  Directory? _dir;
  Future<void>? _starting;

  /// Frames from the engine. Listen after [start].
  Stream<Uint8List> get frames => _requireWorklet().incoming;

  /// Reports that the worklet is gone without [stop] being called, for example a bundle that fails
  /// to load. Listen after [start].
  Stream<WorkletCrash> get crashes => _requireWorklet().onCrash;

  /// Starts the engine. Does nothing if it is already running. Overlapping calls share one start.
  ///
  /// BareWorklet.start needs a file path, not an asset key, so the bundle is copied into a fresh
  /// private temporary directory first. The copy is removed by [stop], by the next start after the
  /// worklet exited on its own, or at once when this start fails.
  Future<void> start() {
    final pending = _starting;
    if (pending != null) return pending;
    final running = _worklet;
    if (running != null && running.state != WorkletState.stopped) {
      return Future<void>.value();
    }

    final attempt = _start();
    _starting = attempt;
    return attempt.whenComplete(() {
      _starting = null;
    });
  }

  Future<void> _start() async {
    // The worklet that used the previous copy is stopped, so nothing reads that copy any more.
    await _removeBundle();
    final data = await rootBundle.load(engineAsset);
    final dir = await Directory.systemTemp.createTemp('holebridge-engine-');
    _dir = dir;
    try {
      final file = File('${dir.path}${Platform.pathSeparator}engine.bundle');
      await file.writeAsBytes(
        data.buffer.asUint8List(data.offsetInBytes, data.lengthInBytes),
        flush: true,
      );
      _worklet = await BareWorklet.start(bundlePath: file.path);
    } catch (_) {
      await _removeBundle();
      rethrow;
    }
  }

  /// Sends one frame to the engine without waiting for it. Throws if the engine was never started.
  /// A send after the engine has stopped fails asynchronously, as a StateError from BareWorklet.
  void send(Uint8List frame) {
    unawaited(_requireWorklet().send(frame));
  }

  /// Stops the engine and removes its bundle copy. The copy is removed even if terminate fails.
  Future<void> stop() async {
    try {
      await _worklet?.terminate();
    } finally {
      _worklet = null;
      await _removeBundle();
    }
  }

  /// Deletes the bundle copy, if there is one. A copy the system already cleared is not an error.
  Future<void> _removeBundle() async {
    final dir = _dir;
    _dir = null;
    if (dir == null) return;
    try {
      await dir.delete(recursive: true);
    } on PathNotFoundException {
      // The system cleared its temporary folder; there is nothing left to remove.
    }
  }

  /// Pauses the engine (the app went to the background). A no-op on desktop.
  Future<void> suspend() => _requireWorklet().suspend();

  /// Resumes a paused engine.
  Future<void> resume() => _requireWorklet().resume();

  BareWorklet _requireWorklet() {
    final worklet = _worklet;
    if (worklet == null) throw StateError('the engine is not started');
    return worklet;
  }
}
