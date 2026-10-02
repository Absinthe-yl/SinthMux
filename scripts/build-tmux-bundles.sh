#!/usr/bin/env bash
# Prepare the tmux builds that the Hub hands to devices without tmux.
#   Linux   : static musl tmux from jpillora/tmux-static-builds (amd64, arm64)
#   macOS   : universal (arm64 + x86_64) tmux built here, libevent linked
#             statically, ncurses from the OS; needs macOS with Xcode CLT
#   Windows : psmux (a native tmux-compatible multiplexer) tmux.exe (x64, arm64)
# Output goes to deploy/bundles/ (ignored by Git) with SHA256SUMS; the Hub
# image copies that directory. Set HTTPS_PROXY if GitHub is slow.
set -euo pipefail

TMUX_VERSION=3.5a
LIBEVENT_VERSION=2.1.12
LINUX_TMUX_RELEASE=v3.5i
PSMUX_VERSION=v3.3.8
MACOS_MIN=11.0

root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/deploy/bundles"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$out"
fetch() { curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 --connect-timeout 20 -o "$2" "$1"; }

printf '== Linux tmux (%s)\n' "$LINUX_TMUX_RELEASE"
for arch in amd64 arm64; do
  fetch "https://github.com/jpillora/tmux-static-builds/releases/download/$LINUX_TMUX_RELEASE/tmux.linux-$arch.stripped.gz" "$work/tmux-linux-$arch.gz"
  gunzip -c "$work/tmux-linux-$arch.gz" > "$out/tmux-linux-$arch"
  chmod 755 "$out/tmux-linux-$arch"
done

printf '== Windows psmux (%s)\n' "$PSMUX_VERSION"
for pair in amd64:x64 arm64:arm64; do
  arch="${pair%%:*}" asset="${pair##*:}"
  fetch "https://github.com/psmux/psmux/releases/download/$PSMUX_VERSION/psmux-$PSMUX_VERSION-windows-$asset.zip" "$work/psmux-$arch.zip"
  unzip -p "$work/psmux-$arch.zip" tmux.exe > "$out/tmux-windows-$arch.exe"
  unzip -p "$work/psmux-$arch.zip" LICENSE > "$out/LICENSE.psmux"
done

if [[ "$(uname -s)" == Darwin ]]; then
  printf '== macOS tmux %s (universal)\n' "$TMUX_VERSION"
  fetch "https://github.com/libevent/libevent/releases/download/release-$LIBEVENT_VERSION-stable/libevent-$LIBEVENT_VERSION-stable.tar.gz" "$work/libevent.tar.gz"
  fetch "https://github.com/tmux/tmux/releases/download/$TMUX_VERSION/tmux-$TMUX_VERSION.tar.gz" "$work/tmux.tar.gz"
  # Some Command Line Tools ship a default SDK newer than their linker; use the
  # first SDK that links for both architectures (override: SINTHMUX_MACOS_SDK).
  printf 'int main(void){return 0;}\n' > "$work/probe.c"
  sdk="${SINTHMUX_MACOS_SDK:-}"
  if [[ -z "$sdk" ]]; then
    for candidate in "$(xcrun --sdk macosx --show-sdk-path)" $(ls -d /Library/Developer/CommandLineTools/SDKs/MacOSX[0-9]*.sdk 2>/dev/null | sort -rV); do
      if cc -arch arm64 -isysroot "$candidate" "$work/probe.c" -o "$work/probe" 2>/dev/null &&
         cc -arch x86_64 -isysroot "$candidate" "$work/probe.c" -o "$work/probe" 2>/dev/null; then
        sdk="$candidate"; break
      fi
    done
  fi
  [[ -n "$sdk" ]] || { printf 'no macOS SDK links for arm64 and x86_64\n' >&2; exit 1; }
  printf 'using SDK %s\n' "$sdk"
  for arch in arm64 x86_64; do
    prefix="$work/prefix-$arch"
    host="$([[ $arch == arm64 ]] && echo aarch64-apple-darwin || echo x86_64-apple-darwin)"
    flags="-arch $arch -mmacosx-version-min=$MACOS_MIN -isysroot $sdk -Os"
    rm -rf "$work/libevent-$LIBEVENT_VERSION-stable" "$work/tmux-$TMUX_VERSION"
    tar -xzf "$work/libevent.tar.gz" -C "$work"
    (cd "$work/libevent-$LIBEVENT_VERSION-stable" &&
      ./configure --host="$host" --prefix="$prefix" --disable-shared --enable-static --disable-openssl --disable-samples --disable-libevent-regress CFLAGS="$flags" >/dev/null &&
      make -j"$(sysctl -n hw.ncpu)" >/dev/null && make install >/dev/null)
    tar -xzf "$work/tmux.tar.gz" -C "$work"
    (cd "$work/tmux-$TMUX_VERSION" &&
      PKG_CONFIG=/usr/bin/false LIBEVENT_CFLAGS="-I$prefix/include" LIBEVENT_LIBS="$prefix/lib/libevent_core.a" \
      LIBEVENT_CORE_CFLAGS="-I$prefix/include" LIBEVENT_CORE_LIBS="$prefix/lib/libevent_core.a" \
      LIBNCURSES_CFLAGS="" LIBNCURSES_LIBS="-lncurses" \
      ./configure --host="$host" --disable-utf8proc CFLAGS="$flags -I$prefix/include" LDFLAGS="-arch $arch -mmacosx-version-min=$MACOS_MIN -isysroot $sdk" >/dev/null &&
      make -j"$(sysctl -n hw.ncpu)" >/dev/null)
    cp "$work/tmux-$TMUX_VERSION/tmux" "$work/tmux-$arch"
    if otool -L "$work/tmux-$arch" | grep -qE '/opt/homebrew|/usr/local'; then
      printf 'macOS tmux links to a non-system library:\n' >&2; otool -L "$work/tmux-$arch" >&2; exit 1
    fi
  done
  lipo -create "$work/tmux-arm64" "$work/tmux-x86_64" -output "$out/tmux-darwin-universal"
  strip -x "$out/tmux-darwin-universal"
  chmod 755 "$out/tmux-darwin-universal"
  tar -xzOf "$work/tmux.tar.gz" "tmux-$TMUX_VERSION/COPYING" > "$out/LICENSE.tmux"
else
  printf '== skip macOS tmux: run this script on a Mac to build it\n'
fi

(cd "$out" && rm -f SHA256SUMS && shasum -a 256 tmux-* > SHA256SUMS && cat SHA256SUMS)
ls -lh "$out"
