import 'dart:convert';
import 'dart:io';

/// Loads spec/vectors/[name] from the repo root. `flutter test` runs with app/ as the working
/// directory, so the shared vectors sit one level up.
Map<String, dynamic> loadVector(String name) {
  final file = File('../spec/vectors/$name');
  if (!file.existsSync()) {
    throw StateError('missing vector file spec/vectors/$name: generate it or check the name');
  }
  return jsonDecode(file.readAsStringSync()) as Map<String, dynamic>;
}
