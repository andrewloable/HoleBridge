// The typed key of the add-host flow (docs/cli.md#the-app, Adding a host).
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

/// The field where the 9 symbols of a host's key are typed: three groups of three, with forgiving
/// input (case, dashes and spaces do not matter, see lib/src/keys/normalize.dart) and controls that a
/// TV remote can reach.
///
/// [onChanged] gets the text as typed after each change. [onSubmitted] runs when the user submits it.
class KeyEntryField extends StatelessWidget {
  const KeyEntryField({super.key, this.onChanged, this.onSubmitted});

  final ValueChanged<String>? onChanged;
  final ValueChanged<String>? onSubmitted;

  @override
  Widget build(BuildContext context) {
    return TextField(
      onChanged: onChanged,
      onSubmitted: onSubmitted,
      inputFormatters: [_KeyFormatter()],
      textAlign: TextAlign.center,
      textCapitalization: TextCapitalization.characters,
      autocorrect: false,
      enableSuggestions: false,
      keyboardType: TextInputType.text,
      textInputAction: TextInputAction.done,
      style: Theme.of(context).textTheme.headlineSmall?.copyWith(letterSpacing: 4),
      decoration: const InputDecoration(
        labelText: 'Key',
        hintText: 'XXX-XXX-XXX',
        border: OutlineInputBorder(),
      ),
    );
  }
}

/// Writes the key as it is typed in its canonical form: upper case, O as 0, I and L as 1 (as
/// normalizeKey reads them), and a dash after every third symbol. Dashes and spaces are dropped and put
/// back in place, and at most 9 symbols are kept. Symbols are counted as code points, as normalizeKey
/// counts them, so a character outside the BMP (two UTF-16 units) is one symbol and is never split.
/// Other symbols are kept as typed, so the controller refuses them with HB-KEY-INVALID. The cursor stays
/// after the same symbol.
class _KeyFormatter extends TextInputFormatter {
  static const _maxSymbols = 9;

  @override
  TextEditingValue formatEditUpdate(TextEditingValue oldValue, TextEditingValue newValue) {
    final cursor = newValue.selection.isValid
        ? newValue.selection.baseOffset
        : newValue.text.length;
    final symbols = <String>[];
    var before = 0; // the symbols kept before the cursor
    var unitsBefore = 0; // their length in UTF-16 units
    var start = 0; // the UTF-16 offset of the rune being read
    for (final rune in newValue.text.runes) {
      final character = String.fromCharCode(rune);
      final at = start;
      start += character.length;
      if (character == '-' || character == ' ') continue;
      if (symbols.length == _maxSymbols) break;
      final symbol = _canonical(character);
      if (at < cursor) {
        before++;
        unitsBefore += symbol.length;
      }
      symbols.add(symbol);
    }

    final formatted = StringBuffer();
    for (var i = 0; i < symbols.length; i++) {
      if (i > 0 && i % 3 == 0) formatted.write('-');
      formatted.write(symbols[i]);
    }
    // The dashes before the cursor: one after each third symbol before it, but not after the last.
    final dashes = before == 0 ? 0 : (before - 1) ~/ 3;
    return TextEditingValue(
      text: formatted.toString(),
      selection: TextSelection.collapsed(offset: unitsBefore + dashes),
    );
  }
}

/// One symbol in canonical form. Only ASCII letters change case, as normalizeKey does.
String _canonical(String symbol) {
  final upper = symbol.codeUnits.every((unit) => unit < 0x80) ? symbol.toUpperCase() : symbol;
  return switch (upper) {
    'O' => '0',
    'I' || 'L' => '1',
    _ => upper,
  };
}
