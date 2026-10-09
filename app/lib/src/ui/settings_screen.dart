// The settings screen: the relay key, Share with my network per host, the VPN mode switch (Android
// only) and the Diagnostics row (docs/cli.md#the-app, Settings). This file is the stub that
// HoleBridge-5vk.13 writes; the behaviour is HoleBridge-5vk.14.
import 'package:flutter/material.dart';

import '../app_controller.dart';
import '../store/host_store.dart';
import '../vpn/vpn_mode_control.dart';

/// The app's settings.
///
/// [controller] carries every engine request the screen causes: AppController.setRelayKey for the relay
/// key and AppController.setShared for Share with my network. The screen sends no request itself. [store]
/// gives the hosts and each host's Share with my network setting (HostStore.shared) to show. [vpn] is VPN
/// mode, whose switch is shown on Android only. [onOpenDiagnostics] opens the diagnostics screen.
class SettingsScreen extends StatelessWidget {
  const SettingsScreen({
    required this.controller,
    required this.store,
    required this.vpn,
    required this.onOpenDiagnostics,
    super.key,
  });

  final AppController controller;
  final HostStore store;
  final VpnModeControl vpn;
  final VoidCallback onOpenDiagnostics;

  @override
  Widget build(BuildContext context) => throw UnimplementedError();
}
