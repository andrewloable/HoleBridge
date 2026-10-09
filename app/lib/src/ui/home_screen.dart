// The app's first screen (docs/cli.md#the-app, Adding a host).
import 'package:flutter/material.dart';

import '../app_controller.dart';
import '../store/host_store.dart';
import 'add_host_screen.dart';

/// Opens on [AddHostScreen] when the store holds no host, which is a fresh install. With a host stored
/// it shows the screen [hostScreenBuilder] builds for the first host. The other parameters are passed
/// on to AddHostScreen unchanged.
class HomeScreen extends StatefulWidget {
  const HomeScreen({
    super.key,
    required this.controller,
    required this.store,
    required this.hostScreenBuilder,
    this.scannerBuilder,
    this.isTv = false,
    this.onAddFromPhone,
    this.initialLink,
    this.onOpenSettings,
  });

  final AppController controller;
  final HostStore store;
  final Widget Function(BuildContext context, String hostId) hostScreenBuilder;
  final ScannerBuilder? scannerBuilder;
  final bool isTv;
  final VoidCallback? onAddFromPhone;
  final String? initialLink;

  /// Passed to the add-host screen, which shows the Settings button while the store holds no host.
  final VoidCallback? onOpenSettings;

  @override
  State<HomeScreen> createState() => _HomeScreenState();
}

class _HomeScreenState extends State<HomeScreen> {
  /// Read once, so a rebuild of the parent does not read the store again.
  late final Future<List<Host>> _hosts = widget.store.hosts();

  @override
  Widget build(BuildContext context) {
    return FutureBuilder<List<Host>>(
      future: _hosts,
      builder: (context, snapshot) {
        if (snapshot.hasError) {
          return const Scaffold(body: Center(child: Text('The stored hosts could not be read.')));
        }
        final hosts = snapshot.data;
        if (hosts == null) {
          return const Scaffold(body: Center(child: CircularProgressIndicator()));
        }
        if (hosts.isEmpty) {
          return AddHostScreen(
            controller: widget.controller,
            store: widget.store,
            hostScreenBuilder: widget.hostScreenBuilder,
            scannerBuilder: widget.scannerBuilder,
            isTv: widget.isTv,
            onAddFromPhone: widget.onAddFromPhone,
            initialLink: widget.initialLink,
            onOpenSettings: widget.onOpenSettings,
          );
        }
        return widget.hostScreenBuilder(context, hosts.first.id);
      },
    );
  }
}
