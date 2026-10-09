package app.holebridge.holebridge.vpn

import io.flutter.plugin.common.MethodChannel
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test

class VpnChannelTest {

    @Test
    fun prepareWhoseConsentScreenThrowsAnswersOnceWithAnErrorAndDoesNotThrow() {
        val host = FakeVpnHost(consent = false)
        host.showConsentError = RuntimeException(THROWN_MARKER)
        val result = FakeResult()

        VpnChannel(host).prepare(result)

        assertEquals(listOf("CONSENT_UNAVAILABLE"), result.errorCodes)
        assertFalse(result.errorMessages.single().orEmpty().contains(THROWN_MARKER))
        assertEquals(0, result.successValues.size)
    }

    @Test
    fun aPrepareAfterAFailedConsentWorksAndLeavesTheFirstResultAlone() {
        val host = FakeVpnHost(consent = false)
        val channel = VpnChannel(host)
        val first = FakeResult()
        host.showConsentError = RuntimeException(THROWN_MARKER)
        channel.prepare(first)

        host.showConsentError = null
        val second = FakeResult()
        channel.prepare(second)

        assertEquals(listOf("CONSENT_UNAVAILABLE"), first.errorCodes)
        assertEquals(0, second.successValues.size + second.errorCodes.size)

        channel.onConsentResult(true)
        assertEquals(listOf<Any?>(true), second.successValues)
        assertEquals(listOf("CONSENT_UNAVAILABLE"), first.errorCodes)

        channel.onConsentResult(true)
        assertEquals(listOf<Any?>(true), second.successValues)
        assertEquals(0, second.errorCodes.size)
    }

    @Test
    fun refusedConsentAnswersFalseExactlyOnce() {
        val host = FakeVpnHost(consent = false)
        val channel = VpnChannel(host)
        val result = FakeResult()

        channel.prepare(result)
        channel.onConsentResult(false)
        channel.onConsentResult(false)

        assertEquals(listOf<Any?>(false), result.successValues)
        assertEquals(0, result.errorCodes.size)
    }

    @Test
    fun aPrepareWhileOneIsPendingSupersedesTheFirstAndLeavesTheSecondPending() {
        val host = FakeVpnHost(consent = false)
        val channel = VpnChannel(host)
        val first = FakeResult()
        val second = FakeResult()

        channel.prepare(first)
        channel.prepare(second)

        assertEquals(listOf("SUPERSEDED"), first.errorCodes)
        assertEquals(0, first.successValues.size)
        assertEquals(0, second.successValues.size + second.errorCodes.size)
        assertEquals(2, host.showCalls)

        channel.onConsentResult(true)

        assertEquals(listOf<Any?>(true), second.successValues)
        assertEquals(listOf("SUPERSEDED"), first.errorCodes)
    }

    @Test
    fun consentAnsweredInsideShowConsentAnswersOnceAndLeavesNothingPending() {
        val host = FakeVpnHost(consent = false)
        val channel = VpnChannel(host)
        host.onShowConsent = { channel.onConsentResult(true) }
        val result = FakeResult()

        channel.prepare(result)
        channel.onConsentResult(true)

        assertEquals(listOf<Any?>(true), result.successValues)
        assertEquals(0, result.errorCodes.size)
        assertEquals(1, host.showCalls)
    }

    private companion object {
        /** Text of an exception thrown by the fake consent screen; must never reach a reply. */
        const val THROWN_MARKER = "no-consent-activity-test-marker"
    }

    private class FakeVpnHost(private val consent: Boolean) : VpnHost {
        var showCalls = 0

        /** When set, showConsent() throws it, as the real one does without a consent activity. */
        var showConsentError: RuntimeException? = null

        /** Runs inside showConsent(), like a consent result that arrives synchronously. */
        var onShowConsent: (() -> Unit)? = null

        override fun hasConsent(): Boolean = consent

        override fun showConsent() {
            showCalls++
            onShowConsent?.invoke()
            showConsentError?.let { throw it }
        }

        override fun startService(socksPort: Int) {}

        override fun stopService() {}

        override fun isServiceRunning(): Boolean = false
    }

    /** Like Flutter's reply: a second success or error call throws, as DartMessenger does. */
    private class FakeResult : MethodChannel.Result {
        val successValues = mutableListOf<Any?>()
        val errorCodes = mutableListOf<String>()
        val errorMessages = mutableListOf<String?>()
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
            reply {
                errorCodes += errorCode
                errorMessages += errorMessage
            }
        }

        override fun notImplemented() {}
    }
}
