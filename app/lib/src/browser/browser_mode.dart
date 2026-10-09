/// How the in-app browser opens a web service on this platform
/// (docs/architecture.md#the-in-app-browser).
enum BrowserMode { separate, shared, system, addressOnly }

/// What the platform offers the in-app browser, from the M1 WebView spike.
class PlatformCaps {
  const PlatformCaps({
    required this.hasWebView,
    required this.perServiceStorage,
    required this.isTv,
    required this.isLinuxDesktop,
  });

  /// A usable WebView exists.
  final bool hasWebView;

  /// The WebView keeps cookies and storage separate per service.
  final bool perServiceStorage;

  /// Android TV or Google TV.
  final bool isTv;

  /// Linux desktop, where Flutter has no first-party WebView.
  final bool isLinuxDesktop;
}

/// Picks the browser mode: separate when per-service storage works, shared when only a WebView
/// exists, system on Linux desktop, addressOnly on a TV without a WebView.
///
/// Without a WebView, a TV gets addressOnly (the tile shows the address). Every other platform
/// gets system, so Linux desktop and a desktop without a WebView2 runtime both open the system
/// browser.
BrowserMode modeFor(PlatformCaps caps) {
  if (caps.hasWebView) {
    return caps.perServiceStorage ? BrowserMode.separate : BrowserMode.shared;
  }
  return caps.isTv ? BrowserMode.addressOnly : BrowserMode.system;
}

/// True when the UI must warn that logins can collide between services.
///
/// Shared mode and system mode both put every service on 127.0.0.1 into one cookie store, since
/// cookies are scoped by host, not port. Separate mode keeps a store per service, and addressOnly
/// opens nothing.
bool warnsAboutLoginCollisions(BrowserMode mode) =>
    mode == BrowserMode.shared || mode == BrowserMode.system;
