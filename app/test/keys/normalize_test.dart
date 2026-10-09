import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/keys/normalize.dart';

import '../helpers/vectors.dart';

void main() {
  final vectors = loadVector('key.json');
  final valid = (vectors['valid'] as List).cast<Map<String, dynamic>>();
  final invalid = (vectors['invalid'] as List).cast<Map<String, dynamic>>();

  group('normalizeKey accepts every valid vector in key.json', () {
    for (final entry in valid) {
      final input = entry['input'] as String;
      final normalized = entry['normalized'] as String;
      test('"$input" normalizes to $normalized', () {
        expect(normalizeKey(input), normalized);
      });
    }
  });

  group('normalizeKey rejects every invalid vector in key.json', () {
    for (final entry in invalid) {
      final input = entry['input'] as String;
      final reason = entry['error'] as String;
      test('"$input" throws KeyFormatException with reason $reason', () {
        expect(
          () => normalizeKey(input),
          throwsA(isA<KeyFormatException>().having((e) => e.reason, 'reason', reason)),
        );
      });
    }
  });

  test('formatKey groups a normalized key as XXX-XXX-XXX', () {
    expect(formatKey('7KQM4X9TR'), '7KQ-M4X-9TR');
  });

  test('isCompleteKey is true for every valid vector in key.json', () {
    for (final entry in valid) {
      expect(isCompleteKey(entry['input'] as String), isTrue);
    }
  });

  test('isCompleteKey is false for every invalid vector in key.json', () {
    for (final entry in invalid) {
      expect(isCompleteKey(entry['input'] as String), isFalse);
    }
  });

  group('edge cases beyond key.json', () {
    Matcher rejectsWith(String reason) =>
        throwsA(isA<KeyFormatException>().having((e) => e.reason, 'reason', reason));

    test('a dotless i is a character fault, not a 1', () {
      expect(() => normalizeKey('7KQM4X9Tı'), rejectsWith('character'));
    });

    test('a symbol outside the BMP counts as one character', () {
      expect(() => normalizeKey('7KQM4X9T\u{1F600}'), rejectsWith('character'));
    });

    test('when both faults are present, length is reported first', () {
      expect(() => normalizeKey('7KQM4X9TU9'), rejectsWith('length'));
    });

    test('formatKey rejects input that is not 9 symbols', () {
      expect(() => formatKey('7KQM4X9T'), throwsArgumentError);
    });
  });
}
