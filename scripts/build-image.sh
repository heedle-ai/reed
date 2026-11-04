#!/usr/bin/env bash
# Builds a reed browser image from a Chromium already installed on this
# Linux machine: the browser's directory, the shared libraries it links,
# fontconfig and fonts, and socat, nothing else (FROM scratch). Prints the
# image ID, which is what programs pin.
#
# Chromium only serves its DevTools protocol on the container's own
# loopback, so reed runs socat from the same image, in the same pod, to
# relay it to a published port.
#
#   scripts/build-image.sh [path/to/chrome-or-chromium]
#
# Without an argument it uses chrome-headless-shell, google-chrome, then
# chromium, from PATH. socat must be installed (apt install socat).
#
# With BUILD_IMAGE_CONTEXT set to an empty folder, it writes the build
# context there (rootfs/ and a Containerfile) and prints that folder
# instead of building. The published image is assembled this way inside a
# Debian container, which has no Podman, and built outside it
# (.github/workflows/image.yml).
set -euo pipefail
bin=${1:-}
if [ -z "$bin" ]; then
  for c in chrome-headless-shell google-chrome chromium chromium-browser; do
    if command -v "$c" >/dev/null; then bin=$(command -v "$c"); break; fi
  done
fi
[ -n "$bin" ] || { echo "build-image: no Chromium found; pass its path" >&2; exit 1; }
# google-chrome on PATH is usually a shell wrapper: follow it to the ELF.
real=$(readlink -f "$bin")
if head -c 2 "$real" | grep -q '#!'; then
  # A wrapper script: Google Chrome's sits next to its binary ("chrome");
  # others end with an `exec /path/to/browser` line.
  here=$(dirname "$real")
  target=""
  for c in chrome chromium chrome-headless-shell; do
    if [ -f "$here/$c" ] && ! head -c 2 "$here/$c" | grep -q '#!'; then target="$here/$c"; break; fi
  done
  if [ -z "$target" ]; then
    target=$(grep -oE '^[[:space:]]*exec[[:space:]]+"?/[^ "]+' "$real" | grep -oE '/[^ "]+' | tail -1 || true)
  fi
  [ -n "$target" ] || { echo "build-image: can't find the browser $bin runs; pass its path" >&2; exit 1; }
  real=$(readlink -f "$target")
  [ -d "$real" ] && real="$real/chrome"
  real=$(readlink -f "$real")
fi
file "$real" | grep -q ELF || { echo "build-image: $real isn't a Chromium binary" >&2; exit 1; }
dir=$(dirname "$real")
name=$(basename "$real")
# The whole folder is copied, so it must be the browser's own folder, not
# something like /usr/bin: Chromium keeps icudtl.dat and .pak files next to
# its binary.
if [ ! -e "$dir/icudtl.dat" ] || ! compgen -G "$dir/*.pak" >/dev/null; then
  echo "build-image: $dir doesn't look like a Chromium folder (no icudtl.dat and .pak files next to $name)" >&2
  exit 1
fi
command -v socat >/dev/null || { echo "build-image: socat isn't installed" >&2; exit 1; }

ctx=${BUILD_IMAGE_CONTEXT:-}
if [ -n "$ctx" ]; then
  if [ -d "$ctx" ] && [ -z "$(ls -A "$ctx")" ]; then work=$ctx; else
    echo "build-image: BUILD_IMAGE_CONTEXT ($ctx) must be an empty folder" >&2; exit 1
  fi
else
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
fi
root="$work/rootfs"
mkdir -p "$root/browser" "$root/tmp" "$root/etc" "$root/home/reed"
chmod 1777 "$root/tmp"
cp -a "$dir/." "$root/browser/"

# Every library the browser and its own .so files load, with the loader.
mkdir -p "$root/usr/bin"
cp -L "$(command -v socat)" "$root/usr/bin/socat"
libs() { for f in "$@"; do ldd "$f" 2>/dev/null | awk '/=> \//{print $3} /^\t\/.*ld-linux/{print $1}'; done; }
mapfile -t sos < <(find "$root/browser" -name '*.so*' -type f)
while IFS= read -r lib; do
  mkdir -p "$root$(dirname "$lib")"
  cp -L "$lib" "$root$lib"
done < <(libs "$real" "$(command -v socat)" "${sos[@]}" | sort -u)
# Libraries Chromium opens at run time rather than linking.
for extra in libnss3 libsoftokn3 libfreeblpriv3 libnssckbi; do
  for f in /usr/lib/x86_64-linux-gnu/$extra.so* /usr/lib/aarch64-linux-gnu/$extra.so* /usr/lib/x86_64-linux-gnu/nss/$extra.so* /usr/lib/aarch64-linux-gnu/nss/$extra.so*; do
    [ -e "$f" ] && mkdir -p "$root$(dirname "$f")" && cp -L "$f" "$root$f"
  done
done
mapfile -t nss < <(find "$root/usr/lib" \( -name 'libsoftokn3*' -o -name 'libfreeblpriv3*' \) 2>/dev/null)
while IFS= read -r lib; do
  [ -e "$root$lib" ] || { mkdir -p "$root$(dirname "$lib")"; cp -L "$lib" "$root$lib"; }
done < <(libs "${nss[@]}" | sort -u)

# Fonts, so pages and screenshots have text.
[ -d /etc/fonts ] && cp -a /etc/fonts "$root/etc/"
mkdir -p "$root/usr/share/fonts"
for d in truetype/dejavu truetype/liberation truetype/liberation2 opentype/noto; do
  [ -d "/usr/share/fonts/$d" ] && mkdir -p "$root/usr/share/fonts/$d" && cp -a "/usr/share/fonts/$d/." "$root/usr/share/fonts/$d/"
done
printf 'root:x:0:0::/:/nonexistent\nreed:x:65534:65534::/home/reed:/nonexistent\n' > "$root/etc/passwd"
printf 'root:x:0:\nreed:x:65534:\n' > "$root/etc/group"
# Writable by the browser's user without chown, so it builds rootless too.
chmod 1777 "$root/home/reed"

# A FROM scratch image has no loader cache: name the library folders.
libpath=$(cd "$root" && find . -name '*.so*' \( -type f -o -type l \) -not -path './browser/*' -printf '/%h\n' | sed 's#^/\./#/#' | sort -u | paste -sd:)
label=$("$real" --version 2>/dev/null | head -1 || true)
label=${label:-unknown}
label=${label//\\/\\\\}
label=${label//\"/\\\"}
cat > "$work/Containerfile" <<CF
FROM scratch
COPY rootfs/ /
USER 65534
ENV HOME=/home/reed
ENV LD_LIBRARY_PATH=/browser:$libpath
LABEL reed.browser="$label"
ENTRYPOINT ["/browser/$name"]
CF
if [ -n "$ctx" ]; then echo "$ctx"; exit 0; fi
podman build --quiet -f "$work/Containerfile" "$work" | tail -1
