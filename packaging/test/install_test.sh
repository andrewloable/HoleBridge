#!/bin/sh
# Tests for packaging/install.sh (TEST task HoleBridge-hb5.11.1; the IMPL task is HoleBridge-hb5.11.2).
#
# Usage: packaging/test/install_test.sh   (from any directory)
#
# The test writes fake release files into a temporary directory, as scripts/release-build.sh writes them:
# the six archives holebridge_<version>_<os>_<arch> for linux and darwin (.tar.gz) and for windows (.zip),
# each on amd64 and arm64, and a SHA256SUMS that lists them in the order of release-build.sh (linux, darwin,
# windows; amd64 then arm64). The tar.gz archives are packed with uid 0 and gid 0, named root, as the real
# ones are. Each fake binary carries a marker line, "# archive: <archive name>", so the test can tell which
# archive install.sh installed. The test serves that directory with python3 -m http.server on 127.0.0.1 and
# a free port, then runs install.sh against it. Each run gets its own directory inside the temporary
# directory, which holds HOME (so the config directory and the default bin directory are inside it), TMPDIR
# and the bin directory. A stub sudo that fails is first on PATH, and the test checks that it never ran. The
# server and the temporary directory are removed on every exit path. The selection cases need a linux or
# darwin test host; the detection case is skipped on a machine that is neither amd64 nor arm64.
#
# The contract with install.sh, beyond docs/cli.md#install:
#   HOLEBRIDGE_BASE_URL  the directory URL that holds SHA256SUMS and the archives (default: the GitHub
#                        release URL). install.sh picks its line of SHA256SUMS by OS and arch.
#   HOLEBRIDGE_BIN_DIR   where the binary goes (default: /usr/local/bin if writable, else ~/.local/bin).
#   HOLEBRIDGE_ARCH      overrides the detected architecture: the test sets it to riscv64 (unsupported), and
#                        to amd64 and arm64 in the selection cases.
#   The config directory is HOLEBRIDGE_CONFIG if set, else $XDG_CONFIG_HOME/holebridge if set, else
#   ~/.config/holebridge. "holebridge app-key --new" creates it (mode 0700) and app.key in it (mode 0600),
#   and refuses when app.key exists. install.sh must leave app.key mode 0600 in a 0700 directory, and must
#   never print the application key.
set -u

root=$(cd "$(dirname "$0")/../.." && pwd)
install_sh="$root/packaging/install.sh"
version=0.0.1-test
passed=0
failed=0
server_pid=
port=

for tool in python3 curl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "install_test: $tool is required" >&2
    exit 1
  fi
done

# tar_bsd is set when tar is bsdtar (macOS). pack_tar uses it to pick the ownership options, with the same
# test as scripts/release-build.sh.
case "$(tar --version 2>/dev/null || true)" in
  *bsdtar*) tar_bsd=1 ;;
  *) tar_bsd= ;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/holebridge-install-test.XXXXXX") || exit 1
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null
    wait "$server_pid" 2>/dev/null
  fi
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

ok() {
  passed=$((passed + 1))
  printf 'ok - %s\n' "$1"
}

# not_ok <name> <reason> [file: its last lines are shown, e.g. what install.sh printed]
not_ok() {
  failed=$((failed + 1))
  printf 'not ok - %s: %s\n' "$1" "$2"
  if [ -n "${3:-}" ] && [ -s "$3" ]; then
    tail -n 5 "$3" | sed 's/^/    /'
  fi
}

fixture_error() {
  echo "install_test: fixture error: $*" >&2
  exit 1
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

# is_empty_dir <dir>: true when the directory is missing or has no entries.
is_empty_dir() {
  [ -z "$(find "$1" -mindepth 1 -print -quit 2>/dev/null)" ]
}

# system_state: whether a holebridge binary sits in the two default install locations of the real
# account. The test must leave both as it found them.
system_state() {
  for p in /usr/local/bin/holebridge "$HOME/.local/bin/holebridge"; do
    if [ -e "$p" ] || [ -L "$p" ]; then
      echo "$p present"
    else
      echo "$p absent"
    fi
  done
}

# pack_tar <archive> <dir>: packs holebridge, LICENSE, THIRD_PARTY_NOTICES.md and licenses/ from <dir> into
# <archive>, with the ownership and member order of scripts/release-build.sh.
pack_tar() {
  if [ -n "$tar_bsd" ]; then
    COPYFILE_DISABLE=1 tar -czf "$1" --uid 0 --gid 0 --uname root --gname root -C "$2" holebridge LICENSE THIRD_PARTY_NOTICES.md licenses
  else
    COPYFILE_DISABLE=1 tar -czf "$1" --owner=0 --group=0 --numeric-owner -C "$2" holebridge LICENSE THIRD_PARTY_NOTICES.md licenses
  fi
}

mkdir -p "$work/serve" "$work/cases" "$work/guard" "$work/pkg" || fixture_error "cannot create the work directories"

# host_os is this host's OS as the archive names spell it, for the selection cases.
case "$(uname -s)" in
  Linux) host_os=linux ;;
  Darwin) host_os=darwin ;;
  *) fixture_error "the selection cases need a linux or darwin host, not $(uname -s)" ;;
esac

# The fake binary: it logs its arguments and answers the commands install.sh uses, as the real CLI does
# (internal/config/config.go Dir, internal/config/appkey.go CreateAppKey, internal/cli/key.go appKeyCmd).
cat > "$work/fake-holebridge" <<'EOF'
#!/bin/sh
# archive: __ARCHIVE__
# Fake holebridge for packaging/test/install_test.sh.
set -eu
echo "$*" >> "$HOME/holebridge-calls.log"
# The config directory, as config.Dir resolves it without --config.
if [ -n "${HOLEBRIDGE_CONFIG:-}" ]; then
  cfg="$HOLEBRIDGE_CONFIG"
elif [ -n "${XDG_CONFIG_HOME:-}" ]; then
  cfg="$XDG_CONFIG_HOME/holebridge"
else
  cfg="$HOME/.config/holebridge"
fi
# create_key: as config.CreateAppKey. The directory is made mode 0700, app.key is created mode 0600 from
# the start, and an existing app.key is never replaced. Nothing is printed on success.
create_key() {
  mkdir -p -m 700 "$cfg"
  if [ -e "$cfg/app.key" ]; then
    echo "app.key already exists" >&2
    exit 1
  fi
  ( umask 077; { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; echo; } > "$cfg/app.key" )
}
case "$*" in
  --version)
    echo "holebridge __VERSION__"
    ;;
  "app-key --new")
    create_key
    ;;
  "app-key")
    # As appKeyCmd: the key is created when it is missing, printed on stdout, and a warning goes to stderr.
    if [ ! -e "$cfg/app.key" ]; then
      create_key
    fi
    cat "$cfg/app.key"
    echo "warning: the application key is a secret" >&2
    ;;
  *)
    echo "fake holebridge: unexpected arguments: $*" >&2
    exit 2
    ;;
esac
EOF

# The stub sudo: if anything runs it, the test sees the marker file.
sudo_marker="$work/sudo-was-run"
printf '#!/bin/sh\n: > "%s"\nexit 99\n' "$sudo_marker" > "$work/guard/sudo" || fixture_error "cannot write the sudo stub"
chmod 755 "$work/guard/sudo"

# make_release <case>: writes the fake archives and SHA256SUMS into serve/<case>, in the order of
# scripts/release-build.sh: linux, darwin and windows, each on amd64 and arm64. Each tar.gz holds holebridge,
# LICENSE, THIRD_PARTY_NOTICES.md and licenses/ at its top level. The windows archives are .zip files of
# dummy bytes: install.sh must pick them by name and must never install one.
make_release() {
  rel="$work/serve/$1"
  mkdir -p "$rel" || fixture_error "cannot create $rel"
  : > "$rel/SHA256SUMS"
  for os in linux darwin windows; do
    for arch in amd64 arm64; do
      name="holebridge_${version}_${os}_${arch}"
      if [ "$os" = windows ]; then
        printf 'not a real zip\n' > "$rel/$name.zip" || fixture_error "cannot write $name.zip"
        (cd "$rel" && sha256_of "$name.zip") >> "$rel/SHA256SUMS" || fixture_error "cannot hash $name.zip"
      else
        pkg="$work/pkg/$name"
        rm -rf "$pkg"
        mkdir -p "$pkg/licenses/go-stdlib" || fixture_error "cannot create $pkg"
        sed -e "s/__VERSION__/$version/" -e "s/__ARCHIVE__/$name/" "$work/fake-holebridge" > "$pkg/holebridge" || fixture_error "cannot write the fake binary"
        chmod 755 "$pkg/holebridge"
        echo "fake LICENSE for $name" > "$pkg/LICENSE"
        echo "fake THIRD_PARTY_NOTICES" > "$pkg/THIRD_PARTY_NOTICES.md"
        echo "fake go-stdlib LICENSE" > "$pkg/licenses/go-stdlib/LICENSE"
        pack_tar "$rel/$name.tar.gz" "$pkg" || fixture_error "cannot pack $name.tar.gz"
        (cd "$rel" && sha256_of "$name.tar.gz") >> "$rel/SHA256SUMS" || fixture_error "cannot hash $name.tar.gz"
      fi
    done
  done
}

# tamper <case>: changes every archive of the case after its SHA256SUMS was written.
tamper() {
  for f in "$work/serve/$1"/*.tar.gz; do
    printf 'tampered\n' >> "$f" || fixture_error "cannot tamper with $f"
  done
}

# new_case <case>: an empty home, bin and TMPDIR for one case, and its release files.
new_case() {
  cdir="$work/cases/$1"
  mkdir -p "$cdir/home" "$cdir/bin" "$cdir/tmp" || fixture_error "cannot create $cdir"
  make_release "$1"
}

# run_install <case> [NAME=value ...]: runs install.sh for one case with a clean environment, so no
# variable of the account leaks in. Its output is appended to <case>/out and <case>/err, so a case that
# runs install.sh twice keeps both runs. Returns its status.
run_install() {
  rcase="$1"
  shift
  rdir="$work/cases/$rcase"
  env -i \
    PATH="$work/guard:$PATH" \
    HOME="$rdir/home" \
    TMPDIR="$rdir/tmp" \
    HOLEBRIDGE_BASE_URL="http://127.0.0.1:$port/$rcase" \
    HOLEBRIDGE_BIN_DIR="$rdir/bin" \
    "$@" \
    sh "$install_sh" >>"$rdir/out" 2>>"$rdir/err"
}

# key_modes <key> <dir>: the modes of app.key and of its directory, as "-rw------- drwx------" (ls -ld).
# shellcheck disable=SC2012 # ls -ld | cut is the mode check that works on Linux and macOS alike
key_modes() {
  echo "$(ls -ld "$1" 2>/dev/null | cut -c1-10) $(ls -ld "$2" 2>/dev/null | cut -c1-10)"
}

# key_file_ok <file>: true when the file is mode 0600 and holds 64 lowercase hex digits and a newline.
# shellcheck disable=SC2012 # ls -ld | cut is the mode check that works on Linux and macOS alike
key_file_ok() {
  [ -f "$1" ] &&
    [ "$(ls -ld "$1" | cut -c1-10)" = "-rw-------" ] &&
    [ "$(wc -c < "$1" | tr -d ' ')" -eq 65 ] &&
    grep -qxE '[0-9a-f]{64}' "$1"
}

# leaks_key <key> <file...>: true when the key value appears in one of the files.
leaks_key() {
  lk_key=$1
  shift
  grep -qF -e "$lk_key" "$@" 2>/dev/null
}

# check_archive <test name> <case> <arch> <status>: install.sh exited 0, and the binary it put in the case's
# bin directory is the one of the <host_os>_<arch> archive, by its marker line.
check_archive() {
  ca_name=$1
  ca_dir="$work/cases/$2"
  ca_want="# archive: holebridge_${version}_${host_os}_$3"
  if [ "$4" -ne 0 ]; then
    not_ok "$ca_name" "install.sh exit status $4" "$ca_dir/err"
  elif [ ! -f "$ca_dir/bin/holebridge" ] || ! grep -qxF "$ca_want" "$ca_dir/bin/holebridge"; then
    ca_got=$(grep -m1 '^# archive: ' "$ca_dir/bin/holebridge" 2>/dev/null)
    not_ok "$ca_name" "the installed binary's marker is '$ca_got', want '$ca_want'" "$ca_dir/err"
  else
    ok "$ca_name"
  fi
}

start_server() {
  port=$(python3 -I -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()') || fixture_error "no free port on 127.0.0.1"
  python3 -I -m http.server --bind 127.0.0.1 --directory "$work/serve" "$port" >"$work/server.log" 2>&1 &
  server_pid=$!
  tries=0
  until curl -fsS --noproxy '*' -o /dev/null "http://127.0.0.1:$port/" 2>/dev/null; do
    tries=$((tries + 1))
    if [ "$tries" -ge 100 ] || ! kill -0 "$server_pid" 2>/dev/null; then
      fixture_error "the local release server did not start (see $work/server.log)"
    fi
    sleep 0.1
  done
}

start_server
system_before=$(system_state)

# 1. A clean install against the local server exits 0 and fetches SHA256SUMS from it.
new_case happy
run_install happy
status=$?
if [ "$status" -ne 0 ]; then
  not_ok "install.sh exits 0 against the local release server" "exit status $status" "$work/cases/happy/err"
elif ! grep -q '"GET /happy/SHA256SUMS ' "$work/server.log"; then
  not_ok "install.sh exits 0 against the local release server" "it did not fetch SHA256SUMS from the local server" "$work/cases/happy/err"
else
  ok "install.sh exits 0 against the local release server"
fi

# 2. The binary is installed in the bin directory and runs.
happy_bin="$work/cases/happy/bin/holebridge"
if [ ! -f "$happy_bin" ] || [ ! -x "$happy_bin" ]; then
  not_ok "the binary is installed and runs" "no executable holebridge in the bin directory" "$work/cases/happy/err"
else
  got=$(HOME="$work/cases/happy/home" "$happy_bin" --version 2>&1)
  if [ "$got" = "holebridge $version" ]; then
    ok "the binary is installed and runs"
  else
    not_ok "the binary is installed and runs" "--version printed '$got', want 'holebridge $version'"
  fi
fi

# 3. A tampered archive fails the checksum, and nothing is installed, configured or run.
new_case tampered
tamper tampered
run_install tampered
status=$?
tdir="$work/cases/tampered"
if [ "$status" -eq 0 ]; then
  not_ok "a tampered archive fails before installing anything" "install.sh exited 0" "$tdir/err"
elif ! grep -qi 'checksum' "$tdir/err"; then
  not_ok "a tampered archive fails before installing anything" "stderr does not report a checksum mismatch" "$tdir/err"
elif ! is_empty_dir "$tdir/bin" || [ -e "$tdir/home/.config/holebridge" ] || [ -e "$tdir/home/holebridge-calls.log" ]; then
  not_ok "a tampered archive fails before installing anything" "something was installed, configured or run" "$tdir/err"
else
  ok "a tampered archive fails before installing anything"
fi

# 4. app.key is created on the first install and left untouched on a re-run.
new_case keys
kdir="$work/cases/keys/home/.config/holebridge"
key="$kdir/app.key"
calls_log="$work/cases/keys/home/holebridge-calls.log"
first=
modes_first=
modes_second=
run_install keys
status=$?
if [ "$status" -ne 0 ]; then
  not_ok "app.key is created on the first install and left untouched on a re-run" "first install exited $status" "$work/cases/keys/err"
elif [ ! -f "$key" ] || [ "$(wc -c < "$key" | tr -d ' ')" -ne 65 ] || ! grep -qxE '[0-9a-f]{64}' "$key"; then
  not_ok "app.key is created on the first install and left untouched on a re-run" "after the first install, app.key is not 64 lowercase hex digits and a newline" "$work/cases/keys/err"
else
  first=$(cat "$key")
  modes_first=$(key_modes "$key" "$kdir")
  run_install keys
  status=$?
  second=$(cat "$key" 2>/dev/null)
  modes_second=$(key_modes "$key" "$kdir")
  calls=$(grep -c 'app-key --new' "$calls_log" 2>/dev/null) || calls=0
  if [ "$status" -ne 0 ]; then
    not_ok "app.key is created on the first install and left untouched on a re-run" "the re-run exited $status" "$work/cases/keys/err"
  elif [ "$first" != "$second" ]; then
    not_ok "app.key is created on the first install and left untouched on a re-run" "the re-run changed app.key"
  elif [ "$calls" -ne 1 ]; then
    not_ok "app.key is created on the first install and left untouched on a re-run" "holebridge app-key --new ran $calls times, want once"
  else
    ok "app.key is created on the first install and left untouched on a re-run"
  fi
fi

# 4a. app.key is mode 0600 in a mode 0700 directory after the first install and after the re-run.
want_modes="-rw------- drwx------"
if [ "$modes_first" = "$want_modes" ] && [ "$modes_second" = "$want_modes" ]; then
  ok "app.key is mode 0600 in a mode 0700 directory after the first install and after the re-run"
else
  not_ok "app.key is mode 0600 in a mode 0700 directory after the first install and after the re-run" "app.key and its directory were '$modes_first' after the first install and '$modes_second' after the re-run, want '$want_modes'" "$work/cases/keys/err"
fi

# 4b. install.sh never prints the application key, in either run. The key is the value of app.key after the
# first install. The failure message does not show the output, so a leaked key is not repeated in the log.
if [ -z "$first" ]; then
  not_ok "install.sh never prints the application key" "app.key was not created, so there is no key to look for" "$work/cases/keys/err"
elif leaks_key "$first" "$work/cases/keys/out" "$work/cases/keys/err"; then
  not_ok "install.sh never prints the application key" "the application key appears in what install.sh printed"
else
  ok "install.sh never prints the application key"
fi

# 5. An unsupported architecture, forced with HOLEBRIDGE_ARCH, exits with a message that names it.
new_case arch
run_install arch HOLEBRIDGE_ARCH=riscv64
status=$?
adir="$work/cases/arch"
if [ "$status" -eq 0 ]; then
  not_ok "an unsupported architecture exits with a clear message" "install.sh exited 0" "$adir/err"
elif ! grep -q 'riscv64' "$adir/err" || ! grep -qi 'unsupported' "$adir/err"; then
  not_ok "an unsupported architecture exits with a clear message" "stderr does not say that riscv64 is unsupported" "$adir/err"
elif ! is_empty_dir "$adir/bin" || [ -e "$adir/home/.config/holebridge" ]; then
  not_ok "an unsupported architecture exits with a clear message" "something was installed or configured" "$adir/err"
else
  ok "an unsupported architecture exits with a clear message"
fi

# 6. shellcheck reports nothing for install.sh.
if ! command -v shellcheck >/dev/null 2>&1; then
  not_ok "shellcheck reports nothing for install.sh" "shellcheck is not installed"
elif ! shellcheck -s sh "$install_sh" >"$work/shellcheck.out" 2>&1; then
  not_ok "shellcheck reports nothing for install.sh" "shellcheck reported problems" "$work/shellcheck.out"
else
  ok "shellcheck reports nothing for install.sh"
fi

# 7. HOLEBRIDGE_ARCH picks the archive of that architecture on this OS.
for want in amd64 arm64; do
  new_case "sel_$want"
  run_install "sel_$want" HOLEBRIDGE_ARCH="$want"
  status=$?
  check_archive "HOLEBRIDGE_ARCH=$want installs the ${host_os}_$want archive" "sel_$want" "$want" "$status"
done

# 8. Without HOLEBRIDGE_ARCH, install.sh detects the machine's architecture and picks that archive.
case "$(uname -m)" in
  x86_64|amd64) detect_arch=amd64 ;;
  aarch64|arm64) detect_arch=arm64 ;;
  *) detect_arch= ;;
esac
if [ -z "$detect_arch" ]; then
  printf 'skip - the detected OS and architecture pick their own archive: uname -m reports %s, which has no fixture\n' "$(uname -m)"
else
  new_case detect
  run_install detect
  status=$?
  check_archive "the detected OS and architecture pick their own archive" detect "$detect_arch" "$status"
fi

# 9. XDG_CONFIG_HOME: app.key goes in $XDG_CONFIG_HOME/holebridge, and nothing is written to ~/.config.
xdg_test="XDG_CONFIG_HOME selects the config directory for app.key"
new_case xdg
xdir="$work/cases/xdg"
xkey="$xdir/xdgcfg/holebridge/app.key"
run_install xdg XDG_CONFIG_HOME="$xdir/xdgcfg"
status=$?
xfirst=
if [ "$status" -ne 0 ]; then
  not_ok "$xdg_test" "first install exited $status" "$xdir/err"
elif ! key_file_ok "$xkey"; then
  not_ok "$xdg_test" "after the first install, $xkey is not mode 0600 with 64 hex digits and a newline" "$xdir/err"
elif [ -e "$xdir/home/.config" ]; then
  not_ok "$xdg_test" "the first install wrote $xdir/home/.config although XDG_CONFIG_HOME is set" "$xdir/err"
else
  xfirst=$(cat "$xkey")
  run_install xdg XDG_CONFIG_HOME="$xdir/xdgcfg"
  status=$?
  xsecond=$(cat "$xkey" 2>/dev/null)
  xcalls=$(grep -c 'app-key --new' "$xdir/home/holebridge-calls.log" 2>/dev/null) || xcalls=0
  if [ "$status" -ne 0 ]; then
    not_ok "$xdg_test" "the re-run exited $status" "$xdir/err"
  elif [ "$xfirst" != "$xsecond" ]; then
    not_ok "$xdg_test" "the re-run changed app.key"
  elif [ "$xcalls" -ne 1 ]; then
    not_ok "$xdg_test" "holebridge app-key --new ran $xcalls times, want once"
  elif leaks_key "$xfirst" "$xdir/out" "$xdir/err"; then
    not_ok "$xdg_test" "the application key appears in what install.sh printed"
  else
    ok "$xdg_test"
  fi
fi

# 10. HOLEBRIDGE_CONFIG wins over XDG_CONFIG_HOME, and neither XDG_CONFIG_HOME nor ~/.config is written.
cfg_test="HOLEBRIDGE_CONFIG selects the config directory for app.key, ahead of XDG_CONFIG_HOME"
new_case cfgenv
cfgcase="$work/cases/cfgenv"
cfgkey="$cfgcase/cfg/app.key"
run_install cfgenv HOLEBRIDGE_CONFIG="$cfgcase/cfg" XDG_CONFIG_HOME="$cfgcase/xdgcfg"
status=$?
cfgfirst=
if [ "$status" -ne 0 ]; then
  not_ok "$cfg_test" "first install exited $status" "$cfgcase/err"
elif ! key_file_ok "$cfgkey"; then
  not_ok "$cfg_test" "after the first install, $cfgkey is not mode 0600 with 64 hex digits and a newline" "$cfgcase/err"
elif [ -e "$cfgcase/xdgcfg/holebridge" ] || [ -e "$cfgcase/home/.config" ]; then
  not_ok "$cfg_test" "the first install wrote to XDG_CONFIG_HOME or to ~/.config" "$cfgcase/err"
else
  cfgfirst=$(cat "$cfgkey")
  run_install cfgenv HOLEBRIDGE_CONFIG="$cfgcase/cfg" XDG_CONFIG_HOME="$cfgcase/xdgcfg"
  status=$?
  cfgsecond=$(cat "$cfgkey" 2>/dev/null)
  cfgcalls=$(grep -c 'app-key --new' "$cfgcase/home/holebridge-calls.log" 2>/dev/null) || cfgcalls=0
  if [ "$status" -ne 0 ]; then
    not_ok "$cfg_test" "the re-run exited $status" "$cfgcase/err"
  elif [ "$cfgfirst" != "$cfgsecond" ]; then
    not_ok "$cfg_test" "the re-run changed app.key"
  elif [ "$cfgcalls" -ne 1 ]; then
    not_ok "$cfg_test" "holebridge app-key --new ran $cfgcalls times, want once"
  elif leaks_key "$cfgfirst" "$cfgcase/out" "$cfgcase/err"; then
    not_ok "$cfg_test" "the application key appears in what install.sh printed"
  else
    ok "$cfg_test"
  fi
fi

# Guard: sudo never ran, and no holebridge binary appeared in a system location.
if [ -e "$sudo_marker" ]; then
  not_ok "nothing is installed outside the temporary directory, and sudo never runs" "the sudo stub was run"
elif [ "$(system_state)" != "$system_before" ]; then
  not_ok "nothing is installed outside the temporary directory, and sudo never runs" "a holebridge binary appeared in a system location"
else
  ok "nothing is installed outside the temporary directory, and sudo never runs"
fi

printf 'install_test: %s passed, %s failed\n' "$passed" "$failed"
if [ "$failed" -ne 0 ]; then
  exit 1
fi
exit 0
