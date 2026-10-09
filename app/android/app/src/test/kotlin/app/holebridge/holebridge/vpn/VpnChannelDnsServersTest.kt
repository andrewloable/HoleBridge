package app.holebridge.holebridge.vpn

import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import org.junit.Assert.assertEquals
import org.junit.Test

class VpnChannelDnsServersTest {

    @Test
    fun dnsServersAnswersTheServersOfTheHostAsAList() {
        val result = FakeResult()

        VpnChannel(DnsHost(listOf("192.168.1.1", "2001:db8::1")))
            .onMethodCall(MethodCall("dnsServers", null), result)

        assertEquals(listOf<Any?>(listOf("192.168.1.1", "2001:db8::1")), result.successValues)
        assertEquals(0, result.errorCodes.size)
    }

    @Test
    fun dnsServersAnswersAnEmptyListWhenTheHostDoesNotOverrideIt() {
        val result = FakeResult()

        VpnChannel(HostWithoutDns()).onMethodCall(MethodCall("dnsServers", null), result)

        assertEquals(listOf<Any?>(emptyList<String>()), result.successValues)
        assertEquals(0, result.errorCodes.size)
    }

    private class DnsHost(private val servers: List<String>) : VpnHost {
        override fun hasConsent(): Boolean = true

        override fun showConsent() {}

        override fun startService(socksPort: Int) {}

        override fun stopService() {}

        override fun isServiceRunning(): Boolean = false

        override fun dnsServers(): List<String> = servers
    }

    /** Relies on the interface default, so it does not override dnsServers(). */
    private class HostWithoutDns : VpnHost {
        override fun hasConsent(): Boolean = true

        override fun showConsent() {}

        override fun startService(socksPort: Int) {}

        override fun stopService() {}

        override fun isServiceRunning(): Boolean = false
    }

    /** Like Flutter's reply: a second success or error call throws, as DartMessenger does. */
    private class FakeResult : MethodChannel.Result {
        val successValues = mutableListOf<Any?>()
        val errorCodes = mutableListOf<String>()
        private var answered = false

        private fun reply(record: () -> Unit) {
            if (answered) throw IllegalStateException("Reply already submitted")
            answered = true
            record()
        }

        override fun success(result: Any?) {
            reply { successValues += result }
        }

        override fun error(errorCode: String, errorMessage: String?, errorDetails: Any?) {
            reply { errorCodes += errorCode }
        }

        override fun notImplemented() {}
    }
}
