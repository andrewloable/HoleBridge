package app.holebridge.holebridge.vpn

import io.flutter.plugin.common.BinaryMessenger
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel

/**
 * What [VpnChannel] needs from Android: the consent screen, the service and the
 * consent result. The activity supplies the real one; tests inject a fake.
 */
interface VpnHost {
    /** True when VpnService.prepare() returns null, that is, the user has approved VPN mode. */
    fun hasConsent(): Boolean

    /** Opens the system consent screen. The outcome comes back to [VpnChannel.onConsentResult]. */
    fun showConsent()

    /** Starts HoleBridgeVpnService in the foreground. */
    fun startService(socksPort: Int)

    /** Stops HoleBridgeVpnService. */
    fun stopService()

    /** True while HoleBridgeVpnService is running. */
    fun isServiceRunning(): Boolean

    /**
     * The DNS servers of the underlying network, in the order the platform lists them.
     * Empty by default, so a host without a network answer still works.
     */
    fun dnsServers(): List<String> = emptyList()
}

/**
 * The method channel "holebridge/vpn" with the methods prepare, start, stop, status and
 * dnsServers. The port given to [start] is the engine's SOCKS5 front port
 * (spec/ipc.md point 7).
 */
class VpnChannel(private val host: VpnHost) : MethodChannel.MethodCallHandler {

    /** The prepare() answer that waits for the consent screen. Main thread only. */
    private var pendingPrepare: MethodChannel.Result? = null

    /** Registers this handler on the channel [CHANNEL_NAME] of [messenger]. */
    fun attach(messenger: BinaryMessenger) {
        MethodChannel(messenger, CHANNEL_NAME).setMethodCallHandler(this)
    }

    override fun onMethodCall(call: MethodCall, result: MethodChannel.Result) {
        when (call.method) {
            "prepare" -> prepare(result)
            "start" -> {
                val socksPort = call.argument<Number>(ARG_SOCKS_PORT)?.toInt()
                if (socksPort == null) {
                    result.error("BAD_ARGUMENT", "start needs $ARG_SOCKS_PORT", null)
                } else {
                    start(socksPort, result)
                }
            }
            "stop" -> stop(result)
            "status" -> status(result)
            "dnsServers" -> result.success(host.dnsServers())
            else -> result.notImplemented()
        }
    }

    /**
     * Answers success at once when consent exists; otherwise shows consent and answers later.
     * If the consent screen cannot open, answers CONSENT_UNAVAILABLE at once, so the pending
     * result is never left answered and still stored.
     */
    fun prepare(result: MethodChannel.Result) {
        if (host.hasConsent()) {
            result.success(true)
            return
        }
        pendingPrepare?.error("SUPERSEDED", "A newer prepare() call replaced this one", null)
        // Stored before showConsent(): the real host may answer synchronously via onConsentResult.
        pendingPrepare = result
        try {
            host.showConsent()
        } catch (e: Exception) {
            // If onConsentResult already answered inside showConsent(), this result is no longer
            // pending and is not answered again. No exception message, stack or argument goes into
            // the error or a log.
            if (pendingPrepare === result) {
                pendingPrepare = null
                result.error("CONSENT_UNAVAILABLE", "The VPN consent screen could not be opened", null)
            }
        }
    }

    /** Called by the activity with the consent outcome. Refusal answers false, not an error. */
    fun onConsentResult(granted: Boolean) {
        val result = pendingPrepare ?: return
        pendingPrepare = null
        result.success(granted)
    }

    /** Starts the service with [socksPort]. Without consent, answers an error that asks for prepare(). */
    fun start(socksPort: Int, result: MethodChannel.Result) {
        if (!host.hasConsent()) {
            result.error("NO_CONSENT", "VPN consent is not granted; call prepare() first", null)
            return
        }
        if (socksPort !in 1..65535) {
            result.error("BAD_PORT", "$ARG_SOCKS_PORT must be 1 to 65535", null)
            return
        }
        host.startService(socksPort)
        result.success(null)
    }

    fun stop(result: MethodChannel.Result) {
        host.stopService()
        result.success(null)
    }

    /** Reports what the app knows: consent, and whether the service runs the stack. */
    fun status(result: MethodChannel.Result) {
        result.success(
            mapOf(
                "consent" to host.hasConsent(),
                "running" to host.isServiceRunning(),
            ),
        )
    }

    companion object {
        const val CHANNEL_NAME = "holebridge/vpn"

        /** The argument key of start that carries the SOCKS5 front port. */
        const val ARG_SOCKS_PORT = "socksPort"
    }
}
