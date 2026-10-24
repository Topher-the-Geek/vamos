#!/bin/sh
# Installs the vamos binary from a GitHub release, checking it against the release's SHA256SUMS,
# then reports the packages vamos needs at run time that this system lacks, with the command that installs them,
# and fails if any is missing, unless --allow-missing, or VAMOS_ALLOW_MISSING=1, is given.
# It never installs packages itself, so a Dockerfile keeps them in its own package step.
#
#   sh install.sh [--version <version>] [--dir <directory>] [--allow-missing]
#
# --version, or VAMOS_VERSION, names the release, such as 0.1.0, or latest; each release's copy of this script
# defaults to that release. --dir, or VAMOS_INSTALL_DIR, is where vamos goes, /usr/local/bin by default.
# VAMOS_RELEASE_URL replaces the release's download URL, such as file:///dist, to install a build before it is released.
# A user other than root installs through sudo.
set -eu

repository=https://github.com/Topher-the-Geek/vamos
# The default on the next line is latest in the repository; the release workflow sets it to the release's version
# in the copy it attaches.
version=${VAMOS_VERSION:-latest}
dir=${VAMOS_INSTALL_DIR:-/usr/local/bin}
allow_missing=${VAMOS_ALLOW_MISSING:-}
usage="usage: sh install.sh [--version <version>] [--dir <directory>] [--allow-missing]"
glibc_minimum=2.34 # the newest symbol version the release binary links; the release smoke test checks it

die() {
	echo "install.sh: $*" >&2
	exit 1
}

# Reports whether there is a CA bundle where Debian's, Fedora's or RHEL's ca-certificates put it,
# where SSL_CERT_FILE, which curl and vamos both read, says it is, or at any path given.
has_ca_bundle() {
	for f in /etc/ssl/certs/ca-certificates.crt /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem \
		/etc/pki/tls/certs/ca-bundle.crt "${SSL_CERT_FILE:-}" "$@"; do
		[ -n "$f" ] && [ -e "$f" ] && return 0
	done
	return 1
}

# Sets packages to the given packages, each after a space and named as Fedora names them, under this distribution's
# names, and fix to the command that installs them, or to nothing on a distribution it doesn't know.
system_packages() {
	# shellcheck source=/dev/null
	ids=$( (. /etc/os-release && echo "$ID ${ID_LIKE:-}") 2>/dev/null || true)
	packages=$1
	case " $ids " in
	*" debian "* | *" ubuntu "*)
		packages=$(echo "$1" | sed 's/ containers-common/ golang-github-containers-common/')
		fix="apt-get update && ${sudo:+sudo }apt-get install -y --no-install-recommends$packages"
		;;
	*" fedora "* | *" rhel "*) fix="dnf install -y$1" ;;
	*) fix= ;;
	esac
	fix=${fix:+${sudo:+sudo }$fix}
}

# Fails for a download that failed, suggesting a missing release only when the server answered 404.
download_failed() {
	[ "${http:-}" != 404 ] || die "$1 doesn't exist; is there a release $version?"
	die "can't download $1"
}

while [ $# -gt 0 ]; do
	case $1 in
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		version=$2
		shift 2
		;;
	--dir)
		[ $# -ge 2 ] || die "--dir needs a value"
		dir=$2
		shift 2
		;;
	--allow-missing)
		allow_missing=1
		shift
		;;
	-h | --help)
		echo "$usage; see the script's header for more"
		exit 0
		;;
	*) die "unknown argument \"$1\"; $usage" ;;
	esac
done

case $allow_missing in
1 | true | yes) allow_missing=1 ;;
"" | 0 | false | no) allow_missing= ;;
*) die "VAMOS_ALLOW_MISSING is \"$allow_missing\"; give 1 to install vamos without the packages it needs" ;;
esac

[ "$(uname -s)" = Linux ] || die "vamos runs only on Linux, not $(uname -s)"

case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "there is no vamos binary for $(uname -m); releases have linux/amd64 and linux/arm64" ;;
esac

# The binary links glibc; musl systems such as Alpine have no GNU_LIBC_VERSION.
glibc=$(getconf GNU_LIBC_VERSION 2>/dev/null | sed -n 's/^glibc //p') || true
[ -n "$glibc" ] || die "vamos needs glibc $glibc_minimum or later, and this system's C library isn't glibc"
if [ "${glibc%%.*}" -lt "${glibc_minimum%%.*}" ] ||
	{ [ "${glibc%%.*}" -eq "${glibc_minimum%%.*}" ] &&
		[ "$(echo "$glibc" | cut -d. -f2)" -lt "${glibc_minimum#*.}" ]; }; then
	die "vamos needs glibc $glibc_minimum or later, and this system has $glibc"
fi

sudo=
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || die "installing to $dir needs root; run this script as root, or install sudo"
	sudo=sudo
fi

case $version in
latest) url=${VAMOS_RELEASE_URL:-$repository/releases/latest/download} ;;
v[0-9]* | [0-9]*) url=${VAMOS_RELEASE_URL:-$repository/releases/download/v${version#v}} ;;
*) die "\"$version\" isn't a version; give one such as 0.1.0, or latest" ;;
esac

# fetch sets http to the HTTP status where it can tell; -S shows curl's own reason for a failure.
if command -v curl >/dev/null 2>&1; then
	fetch() { http=$(curl -fsSL -w '%{http_code}' -o "$2" "$1"); }
elif command -v wget >/dev/null 2>&1; then
	case $url in
	file://*) die "wget can't fetch $url; install curl" ;;
	esac
	fetch() {
		http=
		wget -q -O "$2" "$1"
	}
else
	die "downloading vamos needs curl or wget; install curl, for example with apt-get install -y curl"
fi

# apt-get install --no-install-recommends curl leaves the CA bundle out, and curl's failure wouldn't say so.
case $url in
https://*)
	if ! has_ca_bundle "${CURL_CA_BUNDLE:-}"; then
		system_packages " ca-certificates"
		die "downloading vamos from $url needs a CA bundle, and this system has none;" \
			"install ca-certificates${fix:+ with: $fix}"
	fi
	;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
asset=vamos-linux-$arch
fetch "$url/$asset" "$tmp/$asset" || download_failed "$url/$asset"
fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS" || download_failed "$url/SHA256SUMS"
sum=$(sed -n "s/^\([0-9a-f]\{64\}\)  $asset\$/\1/p" "$tmp/SHA256SUMS")
[ -n "$sum" ] || die "$url/SHA256SUMS has no checksum for $asset"
actual=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
[ "$actual" = "$sum" ] || die "$asset from $url has checksum $actual, but SHA256SUMS says $sum"

$sudo mkdir -p "$dir"
$sudo install -m 0755 "$tmp/$asset" "$dir/vamos"
installed=$("$dir/vamos" --version) || die "the installed $dir/vamos doesn't run"
echo "Installed $installed at $dir/vamos."

# Each check looks for what vamos uses, wherever the distribution's package puts it.
missing=
has_ca_bundle || missing="$missing ca-certificates"
[ -e /etc/containers/policy.json ] || missing="$missing containers-common"
found=
for d in /usr/local/libexec/podman /usr/local/lib/podman /usr/libexec/podman /usr/lib/podman; do
	[ -x "$d/netavark" ] && found=1
done
command -v netavark >/dev/null 2>&1 && found=1
[ -n "$found" ] || missing="$missing netavark"

if [ -n "$missing" ]; then
	system_packages "$missing"
	echo "vamos needs these packages, which aren't installed:$packages" >&2
	if [ -n "$fix" ]; then
		echo "Install them with: $fix" >&2
	else
		echo "Install your distribution's packages of that name." >&2
	fi
fi

echo "vamos runs as root, in a privileged container with BUILDAH_ISOLATION=chroot" \
	"and a volume for /var/lib/containers; see $repository/blob/main/docs/tutorial.md."

# vamos is installed either way, but fails at its first pull or run step without them.
if [ -n "$missing" ]; then
	[ -n "$allow_missing" ] || die "vamos is installed, but can't build without those packages; install them," \
		"or give --allow-missing, or VAMOS_ALLOW_MISSING=1, to install vamos without them"
	echo "Continuing without them, as --allow-missing says." >&2
fi
