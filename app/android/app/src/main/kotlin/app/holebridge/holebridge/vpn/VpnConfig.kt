package app.holebridge.holebridge.vpn

import android.net.LinkProperties
import java.net.InetAddress

/**
 * The VpnService.Builder settings for VPN mode (docs/architecture.md, VPN mode).
 *
 * Only 198.18.0.0/16 is routed into the TUN. The DNS server is 198.18.0.1. The
 * app's own package is disallowed, so the engine's traffic never loops through
 * the VPN. CIDR strings keep the values checkable without the Android framework.
 */
data class VpnConfig(
    val addressCidr: String,
    val routeCidrs: List<String>,
    val dnsServers: List<String>,
    val mtu: Int,
    val sessionName: String,
    val disallowedPackages: List<String>,
) {
    companion object {
        /**
         * Builds the settings. [packageName] is the app's own package, which is
         * disallowed when given.
         */
        fun build(packageName: String? = null): VpnConfig = VpnConfig(
            addressCidr = "198.18.255.254/16",
            routeCidrs = listOf("198.18.0.0/16"),
            dnsServers = listOf("198.18.0.1"),
            mtu = 1500,
            sessionName = "HoleBridge",
            disallowedPackages = listOfNotNull(packageName),
        )
    }
}

/**
 * The DNS servers of the underlying network, as strings, in the order the
 * platform lists them.
 */
fun dnsUpstream(linkProperties: LinkProperties): List<String> = dnsUpstream(linkProperties.dnsServers)

/**
 * The conversion behind [dnsUpstream]. LinkProperties is final, so the unit tests
 * call this overload with the server list instead of a fake LinkProperties.
 */
fun dnsUpstream(servers: List<InetAddress>): List<String> =
    servers.map { requireNotNull(it.hostAddress) { "DNS server without an address" } }
