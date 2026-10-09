import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/errors.g.dart';
import 'package:holebridge/src/ui/error_view.dart';

/// Every text the page shows, joined and lowercased, so a check does not depend on capitals.
String shownText(WidgetTester tester) {
  final parts = [
    for (final widget in tester.widgetList<Text>(find.byType(Text)))
      widget.data ?? widget.textSpan?.toPlainText() ?? '',
  ];
  return parts.join('\n').toLowerCase();
}

void main() {
  final lookup = errorCatalog['HB-LOOKUP-TIMEOUT']!;

  testWidgets('HB-LOOKUP-TIMEOUT shows its catalog problem, cause, fix and code', (tester) async {
    await tester.pumpWidget(
      MaterialApp(home: ErrorView('HB-LOOKUP-TIMEOUT', 'timeout', openLink: (_) async {})),
    );

    final text = shownText(tester);
    expect(text, contains(lookup.problem.toLowerCase()));
    expect(text, contains(lookup.cause.toLowerCase()));
    expect(text, contains(lookup.fix.toLowerCase()));
    expect(text, contains('hb-lookup-timeout'));
  });

  testWidgets('the More link targets https://holebridge.app/errors#hb-lookup-timeout', (
    tester,
  ) async {
    final opened = <Uri>[];
    await tester.pumpWidget(
      MaterialApp(
        home: ErrorView('HB-LOOKUP-TIMEOUT', 'timeout', openLink: (url) async => opened.add(url)),
      ),
    );

    await tester.tap(find.text('More'));
    await tester.pumpAndSettle();

    expect(opened, [Uri.parse('https://holebridge.app/errors#hb-lookup-timeout')]);
  });

  testWidgets('an unknown code shows the generic text and the code', (tester) async {
    const code = 'HB-NOT-A-CODE';
    await tester.pumpWidget(MaterialApp(home: ErrorView(code, 'detail', openLink: (_) async {})));

    final text = shownText(tester);
    expect(text, contains(code.toLowerCase()));
    // The page makes no catalog claim: no other entry's problem shows.
    expect(text, isNot(contains(lookup.problem.toLowerCase())));
    // Some generic text shows besides the code itself.
    expect(text.replaceAll(code.toLowerCase(), '').trim(), isNotEmpty);
  });
}
