package app.holebridge.holebridge.vpn

import io.flutter.plugin.common.MethodChannel
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.net.InetAddress

class VpnConfigTest {

    private val ownPackage = "app.holebridge.holebridge"

    @Test
    fun buildRoutesOnlyTheHoleBridgeRangeAndSetsTheDnsServer() {
        val config = VpnConfig.build()

        assertEquals(listOf("198.18.0.0/16"), config.routeCidrs)
        assertEquals(listOf("198.18.0.1"), config.dnsServers)
    }

    @Test
    fun buildDisallowsTheAppsOwnPackage() {
        val config = VpnConfig.build(ownPackage)

        assertEquals(listOf(ownPackage), config.disallowedPackages)
    }

    @Test
    fun buildNeverAddsADefaultOrIpv6Route() {
        val config = VpnConfig.build(ownPackage)

        assertFalse(config.routeCidrs.any { it.endsWith("/0") })
        assertFalse(config.routeCidrs.any { it.contains(':') })
    }

    @Test
    fun buildSetsTheAddressMtuAndSessionName() {
        val config = VpnConfig.build()

        assertEquals("198.18.255.254/16", config.addressCidr)
        assertEquals(1500, config.mtu)
        assertEquals("HoleBridge", config.sessionName)
    }

    @Test
    fun dnsUpstreamReturnsTheLinkDnsServersInOrder() {
        // The servers a fake LinkProperties would list, in the order it lists them.
        val servers = listOf(InetAddress.getByName("10.0.0.2"), InetAddress.getByName("10.0.0.1"))

        assertEquals(listOf("10.0.0.2", "10.0.0.1"), dnsUpstream(servers))
    }

    @Test
    fun startWithoutConsentReturnsAnErrorAskingForPrepare() {
        val host = FakeVpnHost(consent = false)
        val result = FakeResult()

        VpnChannel(host).start(1080, result)

        assertEquals(1, result.errors.size)
        assertTrue(result.errors[0].orEmpty().contains("prepare"))
        assertEquals(0, result.successes)
        assertEquals(0, host.startCalls)
    }

    private class FakeVpnHost(private val consent: Boolean) : VpnHost {
        var startCalls = 0

        override fun hasConsent(): Boolean = consent

        override fun showConsent() {}

        override fun startService(socksPort: Int) {
            startCalls++
        }

        override fun stopService() {}

        override fun isServiceRunning(): Boolean = false
    }

    private class FakeResult : MethodChannel.Result {
        var successes = 0
        val errors = mutableListOf<String?>()

        override fun success(result: Any?) {
            successes++
        }

        override fun error(errorCode: String, errorMessage: String?, errorDetails: Any?) {
            errors += errorMessage
        }

        override fun notImplemented() {}
    }
}
