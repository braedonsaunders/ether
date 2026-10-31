#!/bin/sh
# Check the Ether container runtime.
#
# With no arguments this only compares the Dockerfiles (packages, uid 999,
# and that they do not return to Bookworm). That text check does not prove
# libva exports vaMapBuffer2.
#
# With --image NAME, run that image and prove the libva it would dlopen
# defines vaMapBuffer2, ffmpeg and ffprobe start and report the pinned
# version, a software encode runs without a GPU, dependent libraries
# resolve, and the ether account is uid/gid 999. The bundled ffmpeg resolves
# that symbol with dlsym, so a Bookworm libva probe must fail the same
# dlsym. No hardware success is claimed.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
symbol=vaMapBuffer2
image=

while [ "$#" -gt 0 ]; do
	case "$1" in
	--image)
		image=$2
		shift 2
		;;
	*)
		echo "usage: check-runtime-stack.sh [--image NAME]" >&2
		exit 2
		;;
	esac
done

fail=0

require() {
	file=$1
	needle=$2
	if ! grep -q -F -- "$needle" "$file"; then
		echo "missing [$needle] in $file" >&2
		fail=1
	fi
}

forbid() {
	file=$1
	needle=$2
	if grep -q -F -- "$needle" "$file"; then
		echo "unexpected [$needle] in $file" >&2
		fail=1
	fi
}

root_df=$root/Dockerfile
amd64_df=$root/packaging/docker/Dockerfile.goreleaser.amd64
arm64_df=$root/packaging/docker/Dockerfile.goreleaser.arm64

for file in "$root_df" "$amd64_df" "$arm64_df"; do
	forbid "$file" 'debian:bookworm'
	require "$file" 'debian:trixie-slim'
	require "$file" 'libva2'
	require "$file" 'libva-drm2'
	require "$file" 'mesa-va-drivers'
	require "$file" 'ocl-icd-libopencl1'
	require "$file" '--gid 999'
	require "$file" '--uid 999'
	forbid "$file" 'libmfx1'
done

require "$root_df" 'TARGETARCH'
require "$root_df" '= amd64'
require "$root_df" 'intel-media-va-driver-non-free'
require "$root_df" 'i965-va-driver-shaders'
require "$root_df" 'libmfx-gen1.2'

require "$amd64_df" 'intel-media-va-driver-non-free'
require "$amd64_df" 'i965-va-driver-shaders'
require "$amd64_df" 'libmfx-gen1.2'

forbid "$arm64_df" 'intel-media-va-driver'
forbid "$arm64_df" 'i965-va-driver'
forbid "$arm64_df" 'libmfx-gen1.2'

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo "docker runtime stack: Dockerfile text matches (this is not a symbol proof)"

if [ -z "$image" ]; then
	exit 0
fi

command -v docker >/dev/null 2>&1 || {
	echo "docker is required for --image" >&2
	exit 1
}
command -v python3 >/dev/null 2>&1 || {
	echo "python3 is required to read ELF symbols" >&2
	exit 1
}

case "$image" in
*[[:space:]]* | *\;* | *\|* | *\`* | *\$*)
	echo "refusing image name: $image" >&2
	exit 2
	;;
esac

work=${TMPDIR:-/tmp}/ether-abi.$$
mkdir -p "$work"
src_name=ether-abi-src-$$
boot_name=ether-abi-boot-$$
probe_tag=ether-abi-bookworm:$$

# The Ether image declares VOLUME /config and /cache, so create/run mint
# anonymous volumes. -v removes only those volumes with the container.
# Do not prune volumes this script did not create.
cleanup() {
	docker rm -fv "$src_name" "$boot_name" >/dev/null 2>&1 || true
	docker rmi "$probe_tag" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

want_ver=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$root/packaging/ffmpeg.lock.json" | head -n 1)
# The lock records the package version (8.1.3-1); ffmpeg -version prints 8.1.3.
want_ff=$(printf '%s' "$want_ver" | sed 's/-[0-9][0-9]*$//')
if [ -z "$want_ff" ]; then
	echo "could not read ffmpeg version from the lock" >&2
	exit 1
fi

echo "runtime check: image $image"

id_out=$(docker run --rm --entrypoint id "$image" ether)
echo "$id_out"
case "$id_out" in
*"uid=999(ether)"*"gid=999(ether)"*) ;;
*)
	echo "ether account is not uid/gid 999" >&2
	exit 1
	;;
esac

check_version() {
	bin=$1
	out=$(docker run --rm --entrypoint "/usr/lib/ether/$bin" "$image" -version 2>&1) || {
		echo "$out" >&2
		echo "$bin -version failed" >&2
		exit 1
	}
	echo "$out" | head -n 2
	printf '%s\n' "$out" | grep -q -F -- "$want_ff" || {
		echo "$bin version does not mention $want_ff" >&2
		exit 1
	}
}

check_version ffmpeg
check_version ffprobe

echo "runtime check: dependent libraries"
for bin in ffmpeg ffprobe ether; do
	echo "--- ldd $bin"
	set +e
	ldd_out=$(docker run --rm --entrypoint /usr/bin/ldd "$image" "/usr/lib/ether/$bin" 2>&1)
	ldd_status=$?
	set -e
	printf '%s\n' "$ldd_out"
	if [ "$bin" = ether ] && printf '%s\n' "$ldd_out" | grep -q "not a dynamic executable"; then
		continue
	fi
	if [ "$ldd_status" -ne 0 ]; then
		echo "ldd $bin failed" >&2
		exit 1
	fi
	if printf '%s\n' "$ldd_out" | grep -q "not found"; then
		echo "unresolved library for $bin" >&2
		exit 1
	fi
	printf '%s\n' "$ldd_out" > "$work/ldd-$bin.txt"
done

# Jellyfin's portable ffmpeg dlopens libva.so.2; ldd does not list it.
libva=$(docker run --rm --entrypoint /bin/sh "$image" -c 'lib=$(ldconfig -p | sed -n "s/.*libva\.so\.2 .* => //p" | head -n 1); readlink -f "$lib"')
if [ -z "$libva" ]; then
	echo "image has no libva.so.2 on the linker path" >&2
	docker run --rm --entrypoint /bin/sh "$image" -c 'find /usr/lib /lib -name "libva.so*" -print'
	exit 1
fi
echo "runtime check: dlopen path $libva"
docker run --rm --entrypoint /bin/sh "$image" -c 'dpkg-query -W libva2 libva-drm2'
image_arch=$(docker run --rm --entrypoint uname "$image" -m)
if [ "$image_arch" = x86_64 ]; then
	docker run --rm --entrypoint /bin/sh "$image" -c 'dpkg-query -W intel-media-va-driver-non-free i965-va-driver-shaders libmfx-gen1.2'
fi

echo "runtime check: packaged library dependencies"
set +e
dep_out=$(docker run --rm --entrypoint /bin/sh "$image" -c 'dpkg -L libva2 libva-drm2 intel-media-va-driver-non-free i965-va-driver-shaders libmfx-gen1.2 mesa-va-drivers ocl-icd-libopencl1 2>/dev/null | while read -r f; do
	case $f in
	*.so|*.so.*)
		if [ -f "$f" ]; then
			printf "=== %s\n" "$f"
			ldd "$f" || true
		fi
		;;
	esac
done')
dep_status=$?
set -e
printf '%s\n' "$dep_out"
if [ "$dep_status" -ne 0 ]; then
	echo "could not list packaged libraries" >&2
	exit 1
fi
if printf '%s\n' "$dep_out" | grep -q "not found"; then
	echo "a packaged library has an unresolved dependency" >&2
	exit 1
fi

docker create --name "$src_name" "$image" >/dev/null
docker cp "$src_name:$libva" "$work/libva.so.2"
docker cp "$src_name:/usr/lib/ether" "$work/bundled"
docker rm -v "$src_name" >/dev/null

sym_line=$(python3 "$root/packaging/docker/elf-dynsym.py" "$symbol" "$work/libva.so.2")
echo "runtime check: $sym_line"
case "$sym_line" in
DEFINED*) ;;
*)
	echo "image libva does not define $symbol" >&2
	exit 1
	;;
esac

contains=$(python3 "$root/packaging/docker/elf-dynsym.py" --contains "$symbol" "$work/bundled/ffmpeg")
echo "runtime check: $contains"
case "$contains" in
CONTAINS*) ;;
*)
	echo "bundled ffmpeg does not reference $symbol" >&2
	exit 1
	;;
esac

echo "runtime check: software smoke (no GPU, no hardware claim)"
docker run --rm --user 999:999 --entrypoint /usr/lib/ether/ffmpeg "$image" \
	-hide_banner -loglevel error \
	-f lavfi -i testsrc2=size=160x120:rate=10:duration=0.2 \
	-f lavfi -i sine=frequency=440:sample_rate=48000:duration=0.2 \
	-c:v libx264 -preset ultrafast -c:a aac -f null -

echo "runtime check: VAAPI init without a device must not miss $symbol"
set +e
va_out=$(docker run --rm --user 999:999 --entrypoint /usr/lib/ether/ffmpeg "$image" \
	-hide_banner -loglevel error \
	-f lavfi -i testsrc2=size=160x120:rate=10:duration=0.2 \
	-vf format=nv12,hwupload \
	-c:v h264_vaapi -f null - 2>&1)
va_status=$?
set -e
printf '%s\n' "$va_out"
if printf '%s\n' "$va_out" | grep -q -e "undefined symbol: $symbol" -e "failed to resolve symbol '$symbol'"; then
	echo "image still reports unresolved $symbol" >&2
	exit 1
fi
if [ "$va_status" -eq 0 ]; then
	echo "runtime check: VAAPI command exited 0; this is not recorded as a GPU pass"
fi

echo "runtime check: Bookworm libva regression"
cp "$root/packaging/docker/va-probe.c" "$work/va-probe.c"
docker run --rm -v "$work:/src" -w /src debian:bookworm-slim \
	sh -c 'apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && gcc -O2 -o va-probe va-probe.c -ldl'
mkdir -p "$work/bookworm/ether"
# The regression only dlopens libva from ffmpeg. Leave the server binary out.
cp "$work/bundled/ffmpeg" "$work/bundled/ffprobe" "$work/bookworm/ether/"
cp "$work/va-probe" "$work/bookworm/va-probe"
cat > "$work/bookworm/Dockerfile" <<'EOF'
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends libva2 libva-drm2 \
 && rm -rf /var/lib/apt/lists/*
COPY ether /usr/lib/ether
COPY va-probe /usr/local/bin/va-probe
RUN chmod 755 /usr/local/bin/va-probe
EOF
docker build -t "$probe_tag" "$work/bookworm"

probe_libva=$(docker run --rm --entrypoint /bin/sh "$probe_tag" -c 'lib=$(ldconfig -p | sed -n "s/.*libva\.so\.2 .* => //p" | head -n 1); readlink -f "$lib"')
if [ -z "$probe_libva" ]; then
	echo "bookworm probe has no libva.so.2 on the linker path" >&2
	exit 1
fi
echo "runtime check: bookworm dlopen path $probe_libva"
docker run --rm --entrypoint /bin/sh "$probe_tag" -c 'dpkg-query -W libva2 libva-drm2'
docker create --name "$src_name" "$probe_tag" >/dev/null
docker cp "$src_name:$probe_libva" "$work/libva-bookworm.so.2"
docker rm -v "$src_name" >/dev/null
probe_sym=$(python3 "$root/packaging/docker/elf-dynsym.py" "$symbol" "$work/libva-bookworm.so.2")
echo "runtime check: $probe_sym"
case "$probe_sym" in
DEFINED*)
	echo "bookworm libva unexpectedly defines $symbol" >&2
	exit 1
	;;
esac

set +e
good_dl=$(docker run --rm -v "$work/va-probe:/usr/local/bin/va-probe:ro" --entrypoint /usr/local/bin/va-probe "$image" 2>&1)
good_dl_status=$?
bad_dl=$(docker run --rm --entrypoint /usr/local/bin/va-probe "$probe_tag" 2>&1)
bad_dl_status=$?
set -e
echo "runtime check: image dlsym status $good_dl_status"
printf '%s\n' "$good_dl"
echo "runtime check: bookworm dlsym status $bad_dl_status"
printf '%s\n' "$bad_dl"
if [ "$good_dl_status" -ne 0 ]; then
	echo "image dlsym of $symbol failed" >&2
	exit 1
fi
if [ "$bad_dl_status" -eq 0 ] || ! printf '%s\n' "$bad_dl" | grep -q "undefined symbol: $symbol"; then
	echo "bookworm probe did not reproduce undefined symbol $symbol" >&2
	exit 1
fi
echo "runtime check: bookworm reproduces undefined symbol $symbol"

echo "runtime check: image boot"
# No network: startup must still reach the listener without a claim or account.
docker run -d --name "$boot_name" --network none "$image" >/dev/null
booted=0
i=0
while [ "$i" -lt 60 ]; do
	if docker logs "$boot_name" 2>&1 | grep -q "Ether listening"; then
		booted=1
		break
	fi
	i=$((i + 1))
	sleep 1
done
echo "runtime check: boot logs"
docker logs "$boot_name" 2>&1 | head -n 80
if [ "$booted" -ne 1 ]; then
	echo "image did not log Ether listening" >&2
	exit 1
fi
uid_line=$(docker exec "$boot_name" /bin/sh -c 'cat /proc/1/status' | awk '/^Uid:/ { print $2; exit }')
echo "runtime check: pid 1 uid $uid_line"
if [ "$uid_line" != 999 ]; then
	echo "entrypoint did not drop to uid 999" >&2
	exit 1
fi
docker stop -t 15 "$boot_name" >/dev/null
docker rm -v "$boot_name" >/dev/null

echo "runtime check: passed for $image (no GPU or Quick Sync claim)"
