# R8 rules for the release build. Flutter's Gradle plugin adds this file to the release build type
# automatically when it exists, so build.gradle.kts does not list it.

# The VPN network stack's JNI binding (app/native, libholebridge_tun.so). The native side looks up
# Java_app_holebridge_holebridge_vpn_TunNative_* by name, and nothing in Kotlin references the class
# until the VpnService is wired up, so R8 would strip it. Keep the class and its members unchanged.
-keep class app.holebridge.holebridge.vpn.TunNative { *; }
