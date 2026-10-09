/// Key normalization for the 9-symbol key (docs/security.md#the-key).
///
/// Thrown by [normalizeKey] when input is not a valid key. [reason] is 'length' when the input does
/// not have 9 symbols, and 'character' when a symbol is outside the Crockford alphabet (U included).
class KeyFormatException implements Exception {
  const KeyFormatException({required this.reason});

  final String reason;
}

const _alphabet = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
const _confusable = {'O': '0', 'I': '1', 'L': '1'};

/// Normalizes typed key input to its 9 canonical symbols. Throws [KeyFormatException].
///
/// Case is ignored, ASCII dashes and spaces are dropped, O reads as 0, and I and L read as 1. Symbols
/// are counted as code points. A length fault is reported before a character fault.
String normalizeKey(String input) {
  final symbols = [
    for (final symbol in input.runes.map(String.fromCharCode))
      if (symbol != '-' && symbol != ' ') _canonical(symbol),
  ];
  if (symbols.length != 9) {
    throw const KeyFormatException(reason: 'length');
  }
  if (!symbols.every((symbol) => _alphabet.contains(symbol))) {
    throw const KeyFormatException(reason: 'character');
  }
  return symbols.join();
}

/// Formats a normalized key as three groups of three, for example 7KQ-M4X-9TR.
String formatKey(String normalized) {
  if (normalized.length != 9) {
    throw ArgumentError('a normalized key has 9 symbols');
  }
  return '${normalized.substring(0, 3)}-${normalized.substring(3, 6)}-${normalized.substring(6)}';
}

/// True when [input] normalizes to a valid key.
bool isCompleteKey(String input) {
  try {
    normalizeKey(input);
    return true;
  } on KeyFormatException {
    return false;
  }
}

/// One symbol in canonical form. Only ASCII letters change case: a non-ASCII character that
/// uppercases to an ASCII one (dotless i, long s) stays outside the alphabet.
String _canonical(String symbol) {
  final upper = symbol.codeUnits.every((unit) => unit < 0x80) ? symbol.toUpperCase() : symbol;
  return _confusable[upper] ?? upper;
}
