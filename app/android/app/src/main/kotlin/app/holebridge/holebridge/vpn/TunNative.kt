package app.holebridge.holebridge.vpn

/**
 * JNI binding for the VPN network stack in libholebridge_tun.so (app/native).
 *
 * The stack reads packets from the VpnService TUN fd and opens each TCP
 * connection or UDP flow as a SOCKS5 request to the engine's front on
 * 127.0.0.1. The caller owns the fd. Stop the stack, wait for
 * [holebridgeTunStop] to return, and only then close the fd.
 *
 * [holebridgeTunStop] blocks until the sessions have closed, so call it off the
 * main thread. A start that fails inside the stack is not reported: the stack's
 * thread just ends, and only the return value of [holebridgeTunStart] is an error.
 */
object TunNative {
    init {
        System.loadLibrary("holebridge_tun")
    }

    /**
     * Starts the stack on its own thread.
     *
     * @param tunFd the TUN fd from VpnService.Builder.establish(), detached or kept open by the caller.
     * @param socksPort the port of the engine's SOCKS5 front on 127.0.0.1.
     * @param mtu the TUN MTU, 576 to 65535.
     * @return 0 when the thread started, otherwise a negative errno: -EINVAL for bad
     *   arguments, -EBADF for an fd that is not open, -EALREADY while a stack is running,
     *   or the error from pthread_create.
     */
    @JvmStatic
    external fun holebridgeTunStart(tunFd: Int, socksPort: Int, mtu: Int): Int

    /** Stops the stack and joins its thread. Does nothing when no stack is running. */
    @JvmStatic
    external fun holebridgeTunStop()
}
