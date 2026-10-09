// D-pad focus for every screen on Android TV (docs/architecture.md#platforms, decisions D24).
import 'package:flutter/services.dart';
import 'package:flutter/widgets.dart';

/// Makes the app's screens work with a TV remote's D-pad. The arrow keys move focus between the controls
/// of a screen, Select (Enter) activates the focused control, Back pops the current route, and the focused
/// control is highlighted.
///
/// Select and Back, and the highlight, come from Flutter's defaults. The one change is for the up and down
/// arrows: Flutter's defaults leave them to a focused text field (for its caret), so a single-line field
/// would keep the remote's focus. Here they move focus even from a text field, so the remote can leave a
/// key field for the control below it.
///
/// It wraps the app's navigator, so every route, pushed or not, gets the same rules
/// (MaterialApp.builder). A screen's first focus lands on its primary action, which the screen marks.
class TvFocusScope extends StatelessWidget {
  const TvFocusScope({super.key, required this.child});

  /// The app's navigator and everything below it.
  final Widget child;

  @override
  Widget build(BuildContext context) => Shortcuts(
    shortcuts: const <ShortcutActivator, Intent>{
      SingleActivator(LogicalKeyboardKey.arrowUp): DirectionalFocusIntent(
        TraversalDirection.up,
        ignoreTextFields: false,
      ),
      SingleActivator(LogicalKeyboardKey.arrowDown): DirectionalFocusIntent(
        TraversalDirection.down,
        ignoreTextFields: false,
      ),
    },
    child: child,
  );
}
