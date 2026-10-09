package app.holebridge.holebridge

import android.app.Activity
import android.content.Intent
import app.holebridge.holebridge.vpn.ActivityVpnHost
import app.holebridge.holebridge.vpn.VpnChannel
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine

class MainActivity : FlutterActivity() {

    /** Answers the consent result once the VPN channel exists. */
    private val vpnHost = ActivityVpnHost(this) { granted -> vpnChannel?.onConsentResult(granted) }

    /** The "holebridge/vpn" channel, created with the Flutter engine. */
    private var vpnChannel: VpnChannel? = null

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        vpnChannel = VpnChannel(vpnHost).also { it.attach(flutterEngine.dartExecutor.binaryMessenger) }
    }

    @Suppress("DEPRECATION") // onActivityResult: the only result route for a FlutterActivity.
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        if (requestCode == ActivityVpnHost.REQUEST_CODE) {
            vpnChannel?.onConsentResult(resultCode == Activity.RESULT_OK)
        } else {
            super.onActivityResult(requestCode, resultCode, data)
        }
    }
}
