# VPN mode's network stack for Android: libholebridge_tun.so.
#
# Built by app/android/app/build.gradle.kts (externalNativeBuild, CMake 3.22.1)
# for arm64-v8a, armeabi-v7a and x86_64. The vendored hev-socks5-tunnel stack
# (netstack/, MIT, see netstack/VERSION) is built as four static libraries, the
# same split as hev's own Android.mk, and linked with the JNI glue in
# jni/holebridge_tun.c into one shared library. The split matters: hev-socks5-core
# and hev-task-system each carry their own rbtree and list code, which only
# links when they are separate archives. The defines below are the ones hev's
# Android.mk and configs.mk pass, and are not changed here.

cmake_minimum_required(VERSION 3.22.1)
project(holebridge_tun C ASM)

set(NETSTACK ${CMAKE_CURRENT_SOURCE_DIR}/netstack)

# Glob, then filter case-sensitively: on a case-insensitive file system the
# *.S glob also matches the per-arch .s files, which the .S file #includes.
file(GLOB_RECURSE HEV_TUNNEL_C CONFIGURE_DEPENDS ${NETSTACK}/src/*.c)
list(FILTER HEV_TUNNEL_C INCLUDE REGEX "\\.c$")
# hev-jni.c holds hev's own JNI names for hev.htproxy.TProxyService. HoleBridge
# uses jni/holebridge_tun.c instead, so it is left out.
list(FILTER HEV_TUNNEL_C EXCLUDE REGEX "/src/hev-jni\\.c$")

file(GLOB_RECURSE HEV_TASK_C CONFIGURE_DEPENDS ${NETSTACK}/third-part/hev-task-system/src/*.c)
list(FILTER HEV_TASK_C INCLUDE REGEX "\\.c$")
file(GLOB_RECURSE HEV_TASK_ASM CONFIGURE_DEPENDS ${NETSTACK}/third-part/hev-task-system/src/*.S)
list(FILTER HEV_TASK_ASM INCLUDE REGEX "\\.S$")

file(GLOB_RECURSE LWIP_C CONFIGURE_DEPENDS ${NETSTACK}/third-part/lwip/src/*.c)
list(FILTER LWIP_C INCLUDE REGEX "\\.c$")

file(GLOB_RECURSE YAML_C CONFIGURE_DEPENDS ${NETSTACK}/third-part/yaml/src/*.c)
list(FILTER YAML_C INCLUDE REGEX "\\.c$")

set(HEV_TASK_DIR ${NETSTACK}/third-part/hev-task-system)
set(LWIP_DIR ${NETSTACK}/third-part/lwip)
set(YAML_DIR ${NETSTACK}/third-part/yaml)

# hev-task-system: configs.mk
add_library(hev_task_system STATIC ${HEV_TASK_C} ${HEV_TASK_ASM})
target_include_directories(hev_task_system PRIVATE
    ${HEV_TASK_DIR}/src ${HEV_TASK_DIR}/include)
target_compile_definitions(hev_task_system PRIVATE
    ENABLE_STACK_OVERFLOW_DETECTION
    ENABLE_MEMALLOC_SLICE
    ENABLE_IO_SPLICE_SYSCALL
    CONFIG_STACK_BACKEND=STACK_MMAP
    CONFIG_STACK_OVERFLOW_DETECTION=1
    CONFIG_MEMALLOC_SLICE_ALIGN=16
    CONFIG_MEMALLOC_SLICE_MAX_SIZE=4096
    CONFIG_MEMALLOC_SLICE_MAX_COUNT=1000
    CONFIG_SCHED_CLOCK=CLOCK_NONE)

# lwip
add_library(hev_lwip STATIC ${LWIP_C})
target_include_directories(hev_lwip PRIVATE
    ${LWIP_DIR}/src/include ${LWIP_DIR}/src/ports/include)
target_compile_definitions(hev_lwip PRIVATE FD_SET_DEFINED SOCKLEN_T_DEFINED)

# yaml: configs.mk
add_library(hev_yaml STATIC ${YAML_C})
target_include_directories(hev_yaml PRIVATE ${YAML_DIR}/src ${YAML_DIR}/include)
target_compile_definitions(hev_yaml PRIVATE
    YAML_VERSION_MAJOR=0
    YAML_VERSION_MINOR=2
    YAML_VERSION_PATCH=5
    YAML_VERSION_STRING="0.2.5")

# hev-socks5-tunnel itself, src/ with its core and misc
add_library(hev_socks5_tunnel STATIC ${HEV_TUNNEL_C})
target_include_directories(hev_socks5_tunnel PRIVATE
    ${NETSTACK}/src
    ${NETSTACK}/src/misc
    ${NETSTACK}/src/core/include
    ${YAML_DIR}/include
    ${LWIP_DIR}/src/include
    ${LWIP_DIR}/src/ports/include
    ${HEV_TASK_DIR}/include)
target_compile_definitions(hev_socks5_tunnel PRIVATE
    # hev-main.c leaves out its own main() when this is set.
    ENABLE_LIBRARY
    FD_SET_DEFINED
    SOCKLEN_T_DEFINED)

# The JNI glue and the shared library the app loads.
add_library(holebridge_tun SHARED ${CMAKE_CURRENT_SOURCE_DIR}/jni/holebridge_tun.c)
target_include_directories(holebridge_tun PRIVATE ${NETSTACK}/src)
target_link_libraries(holebridge_tun PRIVATE hev_socks5_tunnel hev_yaml hev_lwip hev_task_system)

foreach(tgt hev_task_system hev_lwip hev_yaml hev_socks5_tunnel holebridge_tun)
    target_compile_options(${tgt} PRIVATE -O2 -fvisibility=hidden)
    if(ANDROID_ABI STREQUAL "armeabi-v7a")
        target_compile_options(${tgt} PRIVATE -mfpu=neon)
    endif()
    set_target_properties(${tgt} PROPERTIES POSITION_INDEPENDENT_CODE ON)
endforeach()

# 16 KB ELF segment alignment, as hev's build sets it.
target_link_options(holebridge_tun PRIVATE
    -Wl,-z,max-page-size=16384
    -Wl,-z,common-page-size=16384)
