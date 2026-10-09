// What the settings screen needs from VPN mode (docs/architecture.md#vpn-mode-android-and-ios).
// The VpnController of HoleBridge-hb5.5.9 implements it. Its enable asks for consent first and then
// sends vpn.start; a refusal leaves it off.

/// VPN mode as the settings screen sees it. It is injected, so the screen does not depend on the
/// platform channel behind it.
abstract interface class VpnModeControl {
  /// True while VPN mode is on.
  bool get enabled;

  /// Turns VPN mode on: asks for consent, then starts the tunnel.
  Future<void> enable();

  /// Turns VPN mode off: sends vpn.stop and stops the tunnel.
  Future<void> disable();
}
