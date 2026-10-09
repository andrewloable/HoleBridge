// Error display: a code, its catalog text and a link to its entry in the docs (docs/decisions.md#d25,
// docs/errors.md). The caller redacts keys, application keys and addresses from the detail first.
import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';

import '../errors.g.dart';

/// Shows an error's catalog problem, cause and fix (lib/src/errors.g.dart), its code, and a More
/// link to `https://holebridge.app/errors#<lowercase code>`. A code that is not in the catalog shows
/// the code and a generic text, and no More link, because the docs have no entry for it.
///
/// The view scrolls when its text is taller than the space it is given, and the More link sits beside
/// the title, so it stays in view.
///
/// [openLink] opens the More link. The app passes its URL launcher; tests pass a recorder.
class ErrorView extends StatelessWidget {
  const ErrorView(this.code, this.detail, {this.openLink, super.key});

  final String code;
  final String detail;
  final Future<void> Function(Uri url)? openLink;

  @override
  Widget build(BuildContext context) {
    final entry = errorCatalog[code];
    final text = Theme.of(context).textTheme;
    final open = openLink ?? _launch;
    return SingleChildScrollView(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        mainAxisSize: MainAxisSize.min,
        children: [
          Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(entry?.title ?? 'Unknown error', style: text.titleMedium),
                    Text(code, style: text.labelLarge),
                  ],
                ),
              ),
              if (entry != null)
                TextButton(
                  onPressed: () =>
                      open(Uri.parse('https://holebridge.app/errors#${code.toLowerCase()}')),
                  child: const Text('More'),
                ),
            ],
          ),
          if (entry != null) ...[
            _Field('Problem', entry.problem),
            _Field('Cause', entry.cause),
            _Field('Fix', entry.fix),
          ] else
            const Text(
              'This code has no entry in the error catalog. Quote it when you report the problem.',
            ),
          if (detail.isNotEmpty) _Field('Detail', detail),
        ],
      ),
    );
  }
}

/// One labelled part of an error: its name above its text.
class _Field extends StatelessWidget {
  const _Field(this.label, this.body);

  final String label;
  final String body;

  @override
  Widget build(BuildContext context) => Padding(
    padding: const EdgeInsets.only(top: 8),
    child: Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [Text(label, style: Theme.of(context).textTheme.labelMedium), Text(body)],
    ),
  );
}

Future<void> _launch(Uri url) async {
  await launchUrl(url);
}
