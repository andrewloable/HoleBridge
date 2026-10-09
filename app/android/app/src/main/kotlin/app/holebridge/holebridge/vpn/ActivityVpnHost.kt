package app.holebridge.holebridge.vpn

import android.app.Activity
import android.content.Intent
import android.net.ConnectivityManager
import android.net.VpnService

/**
 * The [VpnHost] the activity supplies: the system consent screen, HoleBridgeVpnService and
 * the DNS servers of the active network.
 *
 * When the consent screen opens, its outcome comes back through MainActivity.onActivityResult
 * with [REQUEST_CODE]. When consent already exists at the moment the screen would open, the
 * answer is given at once through [onConsentAnswered].
 */
class ActivityVpnHost(
    private val activity: Activity,
    private val onConsentAnswered: (Boolean) -> Unit,
) : VpnHost {

    override fun hasConsent(): Boolean = VpnService.prepare(activity) == null

    @Suppress("DEPRECATION") // startActivityForResult: the only result route for a FlutterActivity.
    override fun showConsent() {
        val intent = VpnService.prepare(activity)
        if (intent == null) {
            onConsentAnswered(true)
        } else {
            activity.startActivityForResult(intent, REQUEST_CODE)
        }
    }

    override fun startService(socksPort: Int) {
        val intent = Intent(activity, HoleBridgeVpnService::class.java)
            .putExtra(HoleBridgeVpnService.EXTRA_SOCKS_PORT, socksPort)
        activity.startForegroundService(intent)
    }

    override fun stopService() {
        activity.stopService(Intent(activity, HoleBridgeVpnService::class.java))
    }

    override fun isServiceRunning(): Boolean = HoleBridgeVpnService.running

    /**
     * The DNS servers of the active network. Needs ACCESS_NETWORK_STATE in the manifest.
     */
    override fun dnsServers(): List<String> {
        val connectivity = activity.getSystemService(ConnectivityManager::class.java) ?: return emptyList()
        val network = connectivity.activeNetwork ?: return emptyList()
        val linkProperties = connectivity.getLinkProperties(network) ?: return emptyList()
        return dnsUpstream(linkProperties)
    }

    companion object {
        /** The request code of the consent screen. MainActivity compares it in onActivityResult. */
        const val REQUEST_CODE = 0x4842
    }
}
