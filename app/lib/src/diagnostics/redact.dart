// Redaction of diagnostics text (docs/security.md rule 1, docs/cli.md#the-app, decisions D25). Every
// diagnostics text the app copies or reports passes through redact first.

/// What each removed part is replaced with.
const _mark = '[redacted]';

/// A key of 9 Crockford symbols (docs/security.md#the-key), in three groups joined by nothing, a dash or
/// the percent-encoded dash %2D that a key link carries. Case does not matter. The lookarounds keep a
/// match to a whole run of symbols, so a long hex string is not cut into keys.
final _key = RegExp(
  r'(?<![0-9a-z])[0-9a-hjkmnp-tv-z]{3}(?:-|%2d)?[0-9a-hjkmnp-tv-z]{3}(?:-|%2d)?[0-9a-hjkmnp-tv-z]{3}(?![0-9a-z])',
  caseSensitive: false,
);

/// An IPv4 address: four decimal groups of one to three digits.
final _ipv4 = RegExp(r'(?<![0-9])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9])');

/// An IPv6 address in its full or compressed text form. The trailing lookahead makes each alternative
/// take the whole address, so a shorter alternative that would leave digits behind is backtracked.
final _ipv6 = RegExp(
  r'(?<![0-9a-f:])(?:'
  r'(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}'
  r'|(?:[0-9a-f]{1,4}:){1,7}:'
  r'|(?:[0-9a-f]{1,4}:){1,6}:[0-9a-f]{1,4}'
  r'|(?:[0-9a-f]{1,4}:){1,5}(?::[0-9a-f]{1,4}){1,2}'
  r'|(?:[0-9a-f]{1,4}:){1,4}(?::[0-9a-f]{1,4}){1,3}'
  r'|(?:[0-9a-f]{1,4}:){1,3}(?::[0-9a-f]{1,4}){1,4}'
  r'|(?:[0-9a-f]{1,4}:){1,2}(?::[0-9a-f]{1,4}){1,5}'
  r'|[0-9a-f]{1,4}:(?::[0-9a-f]{1,4}){1,6}'
  r'|:(?:(?::[0-9a-f]{1,4}){1,7}|:)'
  r')(?![0-9a-f:])',
  caseSensitive: false,
);

/// Returns [text] with every secret removed: each non-empty string in [secrets], matched exactly as it
/// is written and in any case; every IPv4 and IPv6 address; and every 9-symbol key, dashed or not, in
/// any case, whether or not it is in [secrets]. Where two matches overlap, the whole of both is removed,
/// so no part of a secret survives, whatever the order of [secrets]. Text that is not a secret is left
/// as it is.
String redact(String text, Iterable<String> secrets) {
  final spans = <(int, int)>[];
  void find(RegExp pattern) {
    for (final match in pattern.allMatches(text)) {
      spans.add((match.start, match.end));
    }
  }

  for (final secret in secrets) {
    if (secret.isEmpty) continue;
    // A lookahead matches at every start, so an occurrence that overlaps an earlier one is found too.
    final starts = RegExp('(?=(${RegExp.escape(secret)}))', caseSensitive: false);
    for (final match in starts.allMatches(text)) {
      spans.add((match.start, match.start + match.group(1)!.length));
    }
  }
  find(_ipv6);
  find(_ipv4);
  find(_key);
  if (spans.isEmpty) return text;

  spans.sort((a, b) => a.$1.compareTo(b.$1));
  final out = StringBuffer();
  var copied = 0;
  var i = 0;
  while (i < spans.length) {
    final start = spans[i].$1;
    var end = spans[i].$2;
    i++;
    while (i < spans.length && spans[i].$1 < end) {
      if (spans[i].$2 > end) end = spans[i].$2;
      i++;
    }
    out
      ..write(text.substring(copied, start))
      ..write(_mark);
    copied = end;
  }
  out.write(text.substring(copied));
  return out.toString();
}
