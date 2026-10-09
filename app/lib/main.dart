import 'package:flutter/material.dart';

void main() {
  runApp(const HoleBridgeApp());
}

// The engine (app/engine, started through flutter_pear_bare) comes in a later task.
class HoleBridgeApp extends StatelessWidget {
  const HoleBridgeApp({super.key});

  @override
  Widget build(BuildContext context) {
    return const MaterialApp(
      title: 'HoleBridge',
      home: Scaffold(body: Center(child: Text('HoleBridge'))),
    );
  }
}
