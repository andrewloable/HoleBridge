// lib/src/app.dart types two of its diagnostics version numbers by hand: _appVersion must equal the version in
// pubspec.yaml, and _engineVersion must equal the version in engine/package.json. A release that bumps one and not
// the other fails here. The protocol version is a wire version, not a release number, so it is not checked.
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

String _constant(String source, String name) {
  final match = RegExp("const $name = '([^']*)';").firstMatch(source);
  expect(match, isNotNull, reason: 'lib/src/app.dart declares $name');
  return match!.group(1)!;
}

void main() {
  test('the typed app and engine versions equal their manifests', () {
    // flutter test runs with app/ as the working directory.
    final source = File('lib/src/app.dart').readAsStringSync();

    final pubspec = File('pubspec.yaml').readAsStringSync();
    final pubVersion = RegExp(r'^version: (\S+)$', multiLine: true).firstMatch(pubspec)!.group(1)!;
    // The build number follows the '+' in version: 1.0.0+1, and the app constant holds the name only.
    expect(_constant(source, '_appVersion'), pubVersion.split('+').first);

    final engine = jsonDecode(File('engine/package.json').readAsStringSync()) as Map<String, dynamic>;
    expect(_constant(source, '_engineVersion'), engine['version']);
  });
}
