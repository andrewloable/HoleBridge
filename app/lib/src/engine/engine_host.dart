import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:flutter_pear_bare/flutter_pear_bare.dart';

/// The asset key of the engine bundle, written by tool/copy_engine_bundle.sh.
const engineAsset = 'assets/engine.bundle';

/// The asset key that lists the bundle's native addons, one path per line, each relative to the
/// bundle. The addon files are in [addonsDir] under their base names. Written by
/// tool/copy_engine_bundle.sh.
const addonsList = 'assets/engine_addons/addons.txt';

/// The asset directory that holds the native addon files.
const addonsDir = 'assets/engine_addons/';

/// A bundle copy's name: holebridge-engine-PID-RANDOM, where PID is the app process that wrote it.
final _copyName = RegExp(r'^holebridge-engine-([1-9]\d{0,8})-');

/// Runs HoleBridge's app engine (app/engine) in a Bare worklet and carries raw binary frames to it.
///
/// The frames are HoleBridge's own IPC, decoded by the layer above. This class only starts, stops
/// and pauses the worklet. It uses flutter_pear_bare's BareWorklet, not flutter_pear's Pear class.
class EngineHost {
  BareWorklet? _worklet;
  Directory? _dir;

  /// Every start and stop runs after the one before it, so a stop waits for an in-flight start.
  Future<void> _queue = Future<void>.value();

  /// Frames from the engine. Listen after [start].
  Stream<Uint8List> get frames => _requireWorklet().incoming;

  /// Reports that the worklet is gone without [stop] being called, for example a bundle that fails
  /// to load. Listen after [start].
  Stream<WorkletCrash> get crashes => _requireWorklet().onCrash;

  /// Starts the engine. Does nothing if it is already running. Overlapping calls run one after the
  /// other, so the second one finds the engine running.
  ///
  /// BareWorklet.start needs a file path, not an asset key, so the bundle and its native addons are
  /// copied into a fresh private temporary directory first. The copy is removed by [stop], by the
  /// next start after the worklet exited on its own, or at once when this start fails. Copies that
  /// no running engine can use are removed as well; see [_removeStaleCopies].
  Future<void> start() => _enqueue(_start);

  Future<void> _start() async {
    final running = _worklet;
    if (running != null && running.state != WorkletState.stopped) return;

    // The worklet that used the previous copy is stopped, so nothing reads that copy any more.
    await _removeBundle();
    final data = await rootBundle.load(engineAsset);
    final dir = await Directory.systemTemp.createTemp('holebridge-engine-$pid-');
    _dir = dir;
    final BareWorklet worklet;
    try {
      final file = File('${dir.path}${Platform.pathSeparator}engine.bundle');
      await file.writeAsBytes(
        data.buffer.asUint8List(data.offsetInBytes, data.lengthInBytes),
        flush: true,
      );
      await _writeAddons(dir);
      worklet = await BareWorklet.start(bundlePath: file.path);
    } catch (_) {
      await _removeBundle();
      rethrow;
    }
    _worklet = worklet;
    await _removeStaleCopies(keep: dir, reattached: worklet.reattached);
  }

  /// Sends one frame to the engine without waiting for it. Throws if the engine was never started.
  /// A send after the engine has stopped fails asynchronously, as a StateError from BareWorklet.
  void send(Uint8List frame) {
    unawaited(_requireWorklet().send(frame));
  }

  /// Stops the engine and removes its bundle copy. A start still in flight finishes first, so the
  /// worklet it starts is the one stopped here. The copy is removed even if terminate fails.
  Future<void> stop() => _enqueue(_stop);

  Future<void> _stop() async {
    try {
      await _worklet?.terminate();
    } finally {
      _worklet = null;
      await _removeBundle();
    }
  }

  /// Runs [op] after everything queued before it and returns its result. A failed operation does
  /// not hold up the ones after it.
  Future<void> _enqueue(Future<void> Function() op) {
    final result = _queue.then((_) => op());
    _queue = result.then<void>((_) {}, onError: (_) {});
    return result;
  }

  /// Copies each native addon the bundle loads into the copy's directory, at the path the bundle
  /// loads it from. The bundle reads addons from disk, so they cannot stay in the asset bundle.
  Future<void> _writeAddons(Directory dir) async {
    final listed = await rootBundle.load(addonsList);
    final list = utf8.decode(listed.buffer.asUint8List(listed.offsetInBytes, listed.lengthInBytes));
    for (final rel in const LineSplitter().convert(list)) {
      if (rel.isEmpty) continue;
      final data = await rootBundle.load('$addonsDir${rel.split('/').last}');
      final file = File('${dir.path}/$rel');
      await file.parent.create(recursive: true);
      await file.writeAsBytes(
        data.buffer.asUint8List(data.offsetInBytes, data.lengthInBytes),
        flush: true,
      );
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

  /// Deletes bundle copies that no running engine can be using. A copy of a process that has exited
  /// is stale. A copy of this process is stale only after a fresh boot, because a reattach (a Dart
  /// hot restart) may leave the running worklet on it. Copies of other live processes are never
  /// touched. This assumes one EngineHost per process, as BareWorklet is a process-wide singleton.
  Future<void> _removeStaleCopies({required Directory keep, required bool reattached}) async {
    final List<FileSystemEntity> entries;
    try {
      entries = await Directory.systemTemp.list().toList();
    } on FileSystemException {
      return;
    }
    final keepName = keep.path.split(Platform.pathSeparator).last;
    for (final entry in entries) {
      if (entry is! Directory) continue;
      final name = entry.path.split(Platform.pathSeparator).last;
      final match = _copyName.firstMatch(name);
      if (match == null || name == keepName) continue;
      final owner = int.parse(match.group(1)!);
      final stale = owner == pid ? !reattached : !_isProcessAlive(owner);
      if (!stale) continue;
      try {
        await entry.delete(recursive: true);
      } on FileSystemException {
        // Already removed by someone else; nothing is left to do.
      }
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

/// Whether a process with this pid still exists. dart:io cannot send signal 0, the usual probe, so
/// this sends SIGCONT, which does nothing to a process that is not stopped. Windows has no such
/// probe here, so every other process is taken to be alive and its copy is kept.
bool _isProcessAlive(int owner) =>
    Platform.isWindows || Process.killPid(owner, ProcessSignal.sigcont);
