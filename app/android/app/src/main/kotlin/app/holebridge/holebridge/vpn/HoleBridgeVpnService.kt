package app.holebridge.holebridge.vpn

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.util.Log
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors

/**
 * The native calls the service makes. [TunNativeStack] is the real one, a thin
 * wrapper over [TunNative]. Tests inject a fake through the service constructor.
 */
interface TunStack {
    /** Returns 0 or a negative errno, as [TunNative.holebridgeTunStart] does. */
    fun start(tunFd: Int, socksPort: Int, mtu: Int): Int

    /** Stops the stack and waits for it to finish. */
    fun stop()
}

object TunNativeStack : TunStack {
    override fun start(tunFd: Int, socksPort: Int, mtu: Int): Int =
        TunNative.holebridgeTunStart(tunFd, socksPort, mtu)

    override fun stop() {
        TunNative.holebridgeTunStop()
    }
}

/**
 * The VPN service. It runs in the foreground with a notification, builds the TUN
 * from [VpnConfig] and calls [TunStack.start] with the TUN fd.
 *
 * A start is quick, but a stop blocks until the sessions close, so every stack call
 * runs on [stackThread], one at a time and in order. The TUN fd is closed there as
 * well, after the stop returns, because the stack never closes it.
 */
class HoleBridgeVpnService(
    private val stack: TunStack = TunNativeStack,
) : VpnService() {

    /** The TUN fd this service owns, or null when no tunnel is up. Main thread only. */
    private var tun: ParcelFileDescriptor? = null

    private val stackThread: ExecutorService =
        Executors.newSingleThreadExecutor { Thread(it, "holebridge-tun") }
    private val mainHandler = Handler(Looper.getMainLooper())

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // The system requires startForeground within 5 seconds of startForegroundService,
        // so it comes first, on every start, including one that finds the tunnel already up.
        startForegroundWithNotification()
        if (tun != null) return START_NOT_STICKY

        val socksPort = intent?.getIntExtra(EXTRA_SOCKS_PORT, 0) ?: 0
        if (socksPort !in 1..65535) {
            stopSelf(startId)
            return START_NOT_STICKY
        }

        val config = VpnConfig.build(packageName)
        val fd = establish(config)
        if (fd == null) {
            // Consent was revoked, or another VPN took the device's one VPN slot.
            stopSelf(startId)
            return START_NOT_STICKY
        }
        tun = fd
        stackThread.execute {
            val rc = stack.start(fd.fd, socksPort, config.mtu)
            mainHandler.post { onStackStarted(fd, rc) }
        }
        return START_NOT_STICKY
    }

    /** Main thread. Runs once the stack's start has returned on [stackThread]. */
    private fun onStackStarted(fd: ParcelFileDescriptor, rc: Int) {
        // A stop came first. That stop has already queued the close of this fd.
        if (tun !== fd) return
        if (rc == 0) {
            running = true
        } else {
            // The errno only: no addresses, ports or packets.
            Log.e(TAG, "VPN stack did not start, errno ${-rc}")
            stopTunnel()
        }
    }

    /**
     * Main thread. Stops the stack, then closes the TUN fd. The service ends only
     * after that, so the VPN stays up until its fd is closed.
     */
    private fun stopTunnel() {
        val fd = tun
        tun = null
        running = false
        if (fd == null) {
            stopSelf()
            return
        }
        stackThread.execute {
            stack.stop() // blocks until the sessions have closed
            fd.close() // only now: the stack never closes the fd
            mainHandler.post { if (tun == null) stopSelf() }
        }
    }

    /** Main thread. Returns null when the system does not grant the tunnel. */
    private fun establish(config: VpnConfig): ParcelFileDescriptor? {
        val (address, addressPrefix) = splitCidr(config.addressCidr)
        val builder = Builder()
            .setSession(config.sessionName)
            .setMtu(config.mtu)
            .addAddress(address, addressPrefix)
        for (cidr in config.routeCidrs) {
            val (route, prefix) = splitCidr(cidr)
            builder.addRoute(route, prefix)
        }
        for (server in config.dnsServers) {
            builder.addDnsServer(server)
        }
        for (pkg in config.disallowedPackages) {
            builder.addDisallowedApplication(pkg)
        }
        return builder.establish()
    }

    private fun splitCidr(cidr: String): Pair<String, Int> {
        val (address, prefix) = cidr.split('/')
        return address to prefix.toInt()
    }

    private fun startForegroundWithNotification() {
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(CHANNEL_ID, "VPN mode", NotificationManager.IMPORTANCE_LOW),
        )
        val notification = Notification.Builder(this, CHANNEL_ID)
            .setContentTitle("HoleBridge VPN mode is on")
            .setSmallIcon(applicationInfo.icon)
            .setOngoing(true)
            .build()
        if (Build.VERSION.SDK_INT >= 34) {
            // Must match android:foregroundServiceType="systemExempted" in the manifest.
            startForeground(
                NOTIFICATION_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_SYSTEM_EXEMPTED,
            )
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }

    override fun onDestroy() {
        stopTunnel()
        stackThread.shutdown() // a stop already queued still runs
        super.onDestroy()
    }

    /**
     * The platform may call this on a binder thread, not the main thread (VpnService.onRevoke).
     * stopTunnel() is main thread only, so it runs on the main handler. Do not call
     * super.onRevoke(): it would stop the service before the stack stops and the fd closes.
     */
    override fun onRevoke() {
        mainHandler.post { stopTunnel() }
    }

    companion object {
        /** The intent extra with the engine's SOCKS5 front port, set by the start that launches this service. */
        const val EXTRA_SOCKS_PORT = "socksPort"

        private const val TAG = "HoleBridgeVpn"
        private const val CHANNEL_ID = "holebridge-vpn"
        private const val NOTIFICATION_ID = 1

        /**
         * True while the stack runs: the start returned 0 and no stop has begun.
         * Read by the activity's VpnHost.isServiceRunning().
         */
        @Volatile
        var running: Boolean = false
            private set
    }
}
