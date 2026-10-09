import 'package:flutter_test/flutter_test.dart';

import 'helpers/vectors.dart';

void main() {
  test('example.json loads with both fields', () {
    final vector = loadVector('example.json');
    expect(vector['hello'], 'world');
    expect(vector['bytes'], '00ff');
  });
}
