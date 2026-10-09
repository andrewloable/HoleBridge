/*
 * holebridge_tun.c: JNI entry points for VPN mode's network stack.
 *
 * The Kotlin side is app.holebridge.holebridge.vpn.TunNative. The stack
 * (hev-socks5-tunnel, MIT, vendored under app/native/netstack/) turns the
 * packets read from the VpnService TUN fd into TCP connections and UDP flows,
 * and opens each one as a SOCKS5 request to the engine's front on 127.0.0.1.
 *
 * The caller owns the TUN fd. The stack switches it to non-blocking and never
 * closes it, so the caller closes it after holebridgeTunStop returns.
 *
 * This file never logs. Packet bytes, DNS queries and addresses stay out of
 * every log, so hev's logger is turned off in the config below.
 */

#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include <jni.h>

#include "hev-main.h"

#define TUN_MTU_MIN 576   /* IPv4 minimum datagram size (RFC 791) */
#define TUN_MTU_MAX 65535
#define TUN_CONFIG_SIZE 512

/*
 * One stack at a time. tun_lock guards tun_thread_active and tun_thread.
 * tun_thread_done is set by the thread once hev_socks5_tunnel_main_from_str
 * has returned. tun_config is written under tun_lock before the thread is
 * created, and nothing rewrites it until the thread has been joined.
 */
static pthread_mutex_t tun_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_t tun_thread;
static int tun_thread_active;
static atomic_int tun_thread_done;
static char tun_config[TUN_CONFIG_SIZE];
static size_t tun_config_len;

static int
tun_build_config (int socks_port, int mtu)
{
    int n;

    /*
     * No tunnel name, address or script: the fd comes from VpnService, which
     * sets the address and routes. No mapdns: the engine answers DNS for the
     * service names and forwards the rest. log-file null turns logging off.
     * udp 'udp' is the standard SOCKS5 UDP ASSOCIATE.
     */
    n = snprintf (tun_config, sizeof (tun_config),
                  "tunnel:\n"
                  "  mtu: %d\n"
                  "misc:\n"
                  "  log-file: null\n"
                  "  log-level: warn\n"
                  "socks5:\n"
                  "  address: 127.0.0.1\n"
                  "  port: %d\n"
                  "  udp: 'udp'\n",
                  mtu, socks_port);
    if (n <= 0 || (size_t)n >= sizeof (tun_config))
        return -1;

    tun_config_len = (size_t)n;
    return 0;
}

static void *
tun_thread_main (void *arg)
{
    int tun_fd = (int)(intptr_t)arg;

    /* The result is not kept: hev logs nothing, and a failed start shows up
     * as the thread ending. Kotlin sees no error code after start. */
    (void)hev_socks5_tunnel_main_from_str ((const unsigned char *)tun_config,
                                           (unsigned int)tun_config_len,
                                           tun_fd);

    atomic_store (&tun_thread_done, 1);
    return NULL;
}

JNIEXPORT jint JNICALL
Java_app_holebridge_holebridge_vpn_TunNative_holebridgeTunStart (
    JNIEnv *env, jclass clazz, jint tunFd, jint socksPort, jint mtu)
{
    int rc;

    (void)env;
    (void)clazz;

    if (tunFd < 0 || socksPort < 1 || socksPort > 65535 || mtu < TUN_MTU_MIN ||
        mtu > TUN_MTU_MAX)
        return -EINVAL;

    if (fcntl (tunFd, F_GETFD) < 0)
        return -EBADF;

    pthread_mutex_lock (&tun_lock);

    if (tun_thread_active) {
        if (!atomic_load (&tun_thread_done)) {
            rc = -EALREADY;
            goto out;
        }

        /* The stack ended on its own (an init failure). Reap it and start again. */
        pthread_join (tun_thread, NULL);
        tun_thread_active = 0;
    }

    if (tun_build_config (socksPort, mtu) < 0) {
        rc = -EINVAL;
        goto out;
    }

    atomic_store (&tun_thread_done, 0);
    rc = pthread_create (&tun_thread, NULL, tun_thread_main,
                         (void *)(intptr_t)tunFd);
    if (rc != 0) {
        rc = -rc;
        goto out;
    }

    tun_thread_active = 1;
    rc = 0;

out:
    pthread_mutex_unlock (&tun_lock);
    return rc;
}

JNIEXPORT void JNICALL
Java_app_holebridge_holebridge_vpn_TunNative_holebridgeTunStop (JNIEnv *env,
                                                                 jclass clazz)
{
    (void)env;
    (void)clazz;

    pthread_mutex_lock (&tun_lock);

    if (tun_thread_active) {
        /*
         * Quit only while the stack is still running. hev keeps a stop request
         * that arrives after its teardown, and the next start would then exit
         * at once (checked on the host). A stack that ended on its own has
         * already torn down. The flag is set just after hev returns, so a stop
         * in that instant can still race; only a stack that failed to start
         * can get there. Joining blocks until the sessions have closed.
         */
        if (!atomic_load (&tun_thread_done))
            hev_socks5_tunnel_quit ();

        pthread_join (tun_thread, NULL);
        tun_thread_active = 0;
    }

    pthread_mutex_unlock (&tun_lock);
}
