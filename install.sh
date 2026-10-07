#!/bin/sh
# Payesh one-line installer.
#
#   sudo sh ./install.sh --release-public-key /etc/payesh-release.pub --release-key-id OWNER_SUPPLIED_KEY_ID
#   Legacy unsigned preview: curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --release-mode preview
#
# Downloads the Payesh release for this machine from GitHub Releases, authenticates
# SHA256SUMS with an externally configured Ed25519 anchor, checks every archive, and hands the verified files
# to payesh-install, which creates the services and starts them.
#
# Options (flags, or the matching environment variable):
#   --role ROLE        standalone (default), hub, node, or cli-only   PAYESH_ROLE
#   --version X.Y.Z    release to install (default: latest)          PAYESH_VERSION
#   --release-mode MODE production (default) or unsigned preview     PAYESH_RELEASE_MODE
#   --release-public-key PATH externally trusted Ed25519 PUBLIC KEY PEM PAYESH_RELEASE_PUBLIC_KEY
#   --release-key-id ID externally trusted signing key identifier    PAYESH_RELEASE_KEY_ID
#   --listen ADDR      web listen address (default 0.0.0.0:8787)     PAYESH_LISTEN
#   --domain NAME      get a free HTTPS certificate for NAME          PAYESH_DOMAIN
#   --email ADDR       optional Let's Encrypt contact email           PAYESH_EMAIL
#   --check            only run the host preflight, change nothing
#   --convert-from ROLE explicit hub/standalone to node conversion
#   --transport-url URL destination hub wss URL for conversion
#   --node-identity-file PATH enrolled node identity for conversion
#   --hub-ca-file PATH destination hub CA certificate for conversion
#   --uninstall        stop services and remove the selected role
#   --remove-data      with --uninstall, also delete Payesh data/config/logs
#   --remove-installer with --uninstall, remove payesh-install after cleanup
#
# The dashboard is reachable at http://SERVER_IP:8787 right away, without
# encryption. With --domain (or later: sudo payesh domain NAME) the same port
# switches to HTTPS. Port 443 and existing web servers such as nginx are never
# touched. Port 80 is used briefly to verify the domain if it is free;
# otherwise set PAYESH_CLOUDFLARE_API_TOKEN to verify through Cloudflare DNS.
#
# Private repository (temporary, for testing): export GITHUB_TOKEN with read
# access to the repository and the installer downloads through the GitHub API.
set -eu
PAYESH_RELEASE_BOUND_METADATA_V1=1

REPO="${PAYESH_REPO:-Real-kia/payesh}"
ROLE="${PAYESH_ROLE:-}"
VERSION="${PAYESH_VERSION:-latest}"
LISTEN="${PAYESH_LISTEN:-}"
DOMAIN="${PAYESH_DOMAIN:-}"
EMAIL="${PAYESH_EMAIL:-}"
CHECK_ONLY=0
CONVERT_FROM=""
TRANSPORT_URL=""
NODE_IDENTITY_FILE=""
HUB_CA_FILE=""
UNINSTALL=0
REMOVE_DATA=0
REMOVE_INSTALLER=0
TOKEN="${GITHUB_TOKEN:-}"
RELEASE_MODE="${PAYESH_RELEASE_MODE:-production}"
RELEASE_PUBLIC_KEY="${PAYESH_RELEASE_PUBLIC_KEY:-}"
RELEASE_KEY_ID="${PAYESH_RELEASE_KEY_ID:-}"
RELEASE_CHECKSUMS_SHA256=""

say() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--role) ROLE="${2:?--role needs a value}"; shift 2 ;;
	--role=*) ROLE="${1#*=}"; shift ;;
	--version) VERSION="${2:?--version needs a value}"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--release-mode) RELEASE_MODE="${2:?--release-mode needs a value}"; shift 2 ;;
	--release-public-key) RELEASE_PUBLIC_KEY="${2:?--release-public-key needs a value}"; shift 2 ;;
	--release-checksums-sha256) RELEASE_CHECKSUMS_SHA256="${2:?--release-checksums-sha256 needs a value}"; shift 2 ;;
	--release-key-id) RELEASE_KEY_ID="${2:?--release-key-id needs a value}"; shift 2 ;;
	--listen) LISTEN="${2:?--listen needs a value}"; shift 2 ;;
	--listen=*) LISTEN="${1#*=}"; shift ;;
	--domain) DOMAIN="${2:?--domain needs a value}"; shift 2 ;;
	--domain=*) DOMAIN="${1#*=}"; shift ;;
	--email) EMAIL="${2:?--email needs a value}"; shift 2 ;;
	--email=*) EMAIL="${1#*=}"; shift ;;
	--check) CHECK_ONLY=1; shift ;;
	--convert-from) CONVERT_FROM="${2:?--convert-from needs a value}"; shift 2 ;;
	--transport-url) TRANSPORT_URL="${2:?--transport-url needs a value}"; shift 2 ;;
	--node-identity-file) NODE_IDENTITY_FILE="${2:?--node-identity-file needs a value}"; shift 2 ;;
	--hub-ca-file) HUB_CA_FILE="${2:?--hub-ca-file needs a value}"; shift 2 ;;
	--uninstall) UNINSTALL=1; shift ;;
	--remove-data) REMOVE_DATA=1; shift ;;
	--remove-installer) REMOVE_INSTALLER=1; shift ;;
	-h | --help)
		echo "usage: install.sh [--role standalone|hub|node|cli-only] [--version X.Y.Z] [--release-mode production|preview] [--release-public-key PATH --release-key-id ID] [--listen ADDR] [--domain NAME] [--email ADDR] [--check] [--convert-from hub|standalone --transport-url URL --node-identity-file PATH --hub-ca-file PATH] [--uninstall [--remove-data] [--remove-installer]]"
		exit 0
		;;
	*) die "unknown option: $1 (see --help)" ;;
	esac
done

if [ -n "$CONVERT_FROM" ]; then
	[ "$ROLE" = node ] || die "conversion target must be node"
	[ -n "$TRANSPORT_URL" ] && [ -n "$NODE_IDENTITY_FILE" ] && [ -n "$HUB_CA_FILE" ] || die "conversion needs --transport-url, --node-identity-file, and --hub-ca-file"
fi

UPDATING=0
STATE_FILE=/var/lib/payesh/install-state.json
if [ -f "$STATE_FILE" ]; then
 UPDATING=1
 if [ -z "$ROLE" ]; then
  ROLE="$(sed -n 's/.*"role": *"\([^"]*\)".*/\1/p' "$STATE_FILE" | head -n 1)"
 fi
fi
ROLE="${ROLE:-standalone}"
ACTION=Installing
DONE=installed
if [ "$UPDATING" = 1 ]; then ACTION=Updating; DONE=updated; fi

case "$ROLE" in
standalone | hub | node | cli-only) ;;
*) die "unsupported role '$ROLE' (use standalone, hub, node, or cli-only)" ;;
esac

if [ "$REMOVE_DATA" = 1 ] && [ "$UNINSTALL" != 1 ]; then
	die "--remove-data requires --uninstall"
fi
if [ "$REMOVE_INSTALLER" = 1 ] && [ "$UNINSTALL" != 1 ]; then
	die "--remove-installer requires --uninstall"
fi

if [ "$UNINSTALL" = 1 ]; then
	[ "$(id -u)" -eq 0 ] || die "please run as root, e.g. sudo ./install.sh --uninstall"
	UNINSTALLER="${PAYESH_INSTALLER:-/usr/bin/payesh-install}"
	[ -x "$UNINSTALLER" ] || die "installed payesh-install not found at $UNINSTALLER; run it directly or set PAYESH_INSTALLER"
	set -- --role "$ROLE" --uninstall
	[ "$REMOVE_DATA" = 0 ] || set -- "$@" --remove-data
	[ "$REMOVE_INSTALLER" = 0 ] || set -- "$@" --remove-installer
	exec "$UNINSTALLER" "$@"
fi

VERSION="${VERSION#v}"

case "$RELEASE_MODE" in
production)
 [ -n "$RELEASE_PUBLIC_KEY" ] && [ -n "$RELEASE_KEY_ID" ] || die "production releases require an externally configured --release-public-key and --release-key-id; unsigned legacy releases require explicit --release-mode preview"
 [ -f "$RELEASE_PUBLIC_KEY" ] && [ ! -L "$RELEASE_PUBLIC_KEY" ] || die "release public key must be a regular non-symlink PEM file"
 case "$RELEASE_KEY_ID" in ''|*[!A-Za-z0-9._-]*|[._-]*|unavailable-local) die "invalid production release key ID" ;; esac
 [ "${#RELEASE_KEY_ID}" -le 128 ] || die "release key ID is too long"
 command -v openssl >/dev/null 2>&1 || die "production verification requires OpenSSL with Ed25519 pkeyutl -rawin support (OpenSSL 3 or newer)"
 # Root may trust its own anchor, never a service-account-owned or writable file.
 [ "$(stat -c %u "$RELEASE_PUBLIC_KEY")" = 0 ] || die "production public key must be owned by root"
 key_mode="$(stat -c %a "$RELEASE_PUBLIC_KEY")"
 [ "$((0$key_mode & 022))" -eq 0 ] || die "production public key must not be group/world writable"
 key_dir="$(cd "$(dirname "$RELEASE_PUBLIC_KEY")" && pwd -P)"
 while :; do
  [ "$(stat -c %u "$key_dir")" = 0 ] || die "production anchor directories must be owned by root"
  dir_mode="$(stat -c %a "$key_dir")"
  if [ "$((0$dir_mode & 022))" -ne 0 ] && [ "$((0$dir_mode & 01000))" -eq 0 ]; then
   die "production anchor directory is group/world writable without sticky ownership protection"
  fi
  [ "$key_dir" != / ] || break
  key_dir="$(dirname "$key_dir")"
 done
 openssl pkey -pubin -in "$RELEASE_PUBLIC_KEY" -text -noout 2>/dev/null | head -n 1 | grep -q '^ED25519 Public-Key:' || die "production public key must be an Ed25519 PUBLIC KEY PEM"
 ;;
preview)
 [ -z "$RELEASE_PUBLIC_KEY$RELEASE_KEY_ID" ] || die "preview mode does not accept production trust inputs"
 warn "UNSIGNED PREVIEW: SHA256SUMS checks byte integrity only; release publisher authenticity is not verified"
 ;;
*) die "release mode must be production or preview" ;;
esac

# --- host checks -----------------------------------------------------------

[ "$(uname -s)" = Linux ] || die "Payesh runs on Linux servers only."
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported CPU architecture: $(uname -m) (need x86_64 or aarch64)" ;;
esac
[ "$(id -u)" -eq 0 ] || die "please run as root, e.g.: curl -fsSL <installer-url> | sudo sh"

for tool in tar gzip; do
	command -v "$tool" >/dev/null 2>&1 || die "'$tool' is required but not installed."
done
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 | cut -d' ' -f1; }
else
	die "'sha256sum' is required but not installed."
fi
if command -v curl >/dev/null 2>&1; then
	HAVE_CURL=1
elif command -v wget >/dev/null 2>&1; then
	HAVE_CURL=0
	[ -z "$TOKEN" ] || die "installing from a private repository needs curl."
else
	die "'curl' or 'wget' is required but not installed."
fi

# download URL OUTPUT [ACCEPT]
# With a token, requests go through the GitHub API. curl drops the
# Authorization header when the API redirects an asset to its storage host.
download() {
	if [ "$HAVE_CURL" = 1 ]; then
		if [ -n "$TOKEN" ]; then
			curl -fsSL --proto '=https' --tlsv1.2 --retry 3 \
				-H "Authorization: Bearer $TOKEN" \
				-H "Accept: ${3:-application/vnd.github+json}" \
				-H "X-GitHub-Api-Version: 2022-11-28" \
				-o "$2" "$1"
		else
			curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$2" "$1"
		fi
	else
		wget -q -O "$2" "$1"
	fi
}

WORK="$(mktemp -d /tmp/payesh-install.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT
trap 'exit 130' INT TERM
ARTIFACTS="$WORK/artifacts"
mkdir -p "$WORK/archives" "$ARTIFACTS"

# --- locate the release ----------------------------------------------------

# Signature envelopes bind an exact version; resolve latest before downloading.
if [ "$RELEASE_MODE" = production ] && [ "$VERSION" = latest ] && [ -z "$TOKEN" ]; then
 download "https://api.github.com/repos/$REPO/releases/latest" "$WORK/latest.json" || die "could not resolve latest production release"
 VERSION="$(tr ',' '\n' <"$WORK/latest.json" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)"
 [ -n "$VERSION" ] || die "latest release has no version"
fi

if [ -n "$TOKEN" ]; then
	: # Use authenticated downloads without printing repository mode.
	if [ "$VERSION" = latest ]; then
		api="https://api.github.com/repos/$REPO/releases/latest"
	else
		api="https://api.github.com/repos/$REPO/releases/tags/v$VERSION"
	fi
	download "$api" "$WORK/release.json" ||
		die "could not read the release from $api (check the token's repository access and that a release exists)."
	# Each asset lists its API "url" before its "name". Split the JSON into
	# one field per line, remember the last asset URL, and emit it when the
	# name follows.
	tr ',{}[]' '\n' <"$WORK/release.json" | awk '
		/"url": *"https:\/\/api\.github\.com\/repos\/.*\/releases\/assets\/[0-9]+"/ {
			match($0, /https:[^"]+/); url = substr($0, RSTART, RLENGTH); next
		}
		/"name": *"/ && url != "" {
			match($0, /"name": *"[^"]+"/); n = substr($0, RSTART, RLENGTH)
			sub(/"name": *"/, "", n); sub(/"$/, "", n)
			print n, url; url = ""
		}' >"$WORK/assets"
	[ -s "$WORK/assets" ] || die "the release has no downloadable files."
	if [ "$VERSION" = latest ]; then
		VERSION="$(tr ',' '\n' <"$WORK/release.json" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)"
	fi
	get_asset() {
		url="$(awk -v n="$1" '$1 == n { print $2; exit }' "$WORK/assets")"
		[ -n "$url" ] || die "release v$VERSION has no file named $1."
		download "$url" "$2" application/octet-stream
	}
else
	if [ "$VERSION" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		base="https://github.com/$REPO/releases/download/v$VERSION"
	fi
	get_asset() {
		download "$base/$1" "$2" ||
			die "could not download $base/$1 (is there a published release? for a private repository set GITHUB_TOKEN)."
	}
fi

# --- download and verify ---------------------------------------------------

case "$ROLE" in
standalone | hub) NEEDED="payesh-agent payesh-privd payesh payesh-server web-assets" ;;
node) NEEDED="payesh-agent payesh-privd payesh" ;;
cli-only) NEEDED="payesh" ;;
esac

say "$ACTION Payesh ${VERSION:-latest} ($ROLE, linux/$ARCH) from github.com/$REPO"
get_asset SHA256SUMS "$WORK/SHA256SUMS"
if [ "$RELEASE_MODE" = production ]; then
 case "$VERSION" in ''|*[!0-9A-Za-z.-]*) die "invalid production release version" ;; esac
 get_asset SHA256SUMS.sig "$WORK/SHA256SUMS.sig"
 [ "$(wc -c <"$WORK/SHA256SUMS" | tr -d ' ')" -le 1048576 ] || die "production checksum index is oversized"
 signature="$(cat "$WORK/SHA256SUMS.sig")"
 [ "${#signature}" -eq 86 ] || die "invalid bootstrap signature length"
 case "$signature" in *[!A-Za-z0-9_-]*) die "invalid bootstrap signature encoding" ;; esac
 [ "$(wc -c <"$WORK/SHA256SUMS.sig" | tr -d ' ')" -eq 86 ] || die "bootstrap signature must not contain whitespace"
 printf '%s==' "$signature" | tr '_-' '/+' | openssl base64 -d -A >"$WORK/bootstrap.sig" || die "could not decode bootstrap signature"
 [ "$(wc -c <"$WORK/bootstrap.sig" | tr -d ' ')" -eq 64 ] || die "invalid decoded bootstrap signature"
 { printf 'payesh.checksums.v1\n%s\n%s\n' "$VERSION" "$RELEASE_KEY_ID"; cat "$WORK/SHA256SUMS"; } >"$WORK/bootstrap.payload"
 openssl pkeyutl -verify -pubin -inkey "$RELEASE_PUBLIC_KEY" -rawin -in "$WORK/bootstrap.payload" -sigfile "$WORK/bootstrap.sig" >/dev/null 2>&1 || die "production release signature verification failed (or installed OpenSSL lacks Ed25519 support); refusing to extract or execute release files"
 say "Production release checksum signature verified"
fi

# A root worker preflight authorized one exact manifest/index generation.
if [ -n "$RELEASE_CHECKSUMS_SHA256" ]; then
 [ "$RELEASE_MODE" = production ] || die "bound metadata requires production mode"
 [ "${#RELEASE_CHECKSUMS_SHA256}" -eq 64 ] || die "invalid bound checksum index digest"
 case "$RELEASE_CHECKSUMS_SHA256" in *[!0-9a-f]*) die "invalid bound checksum index digest" ;; esac
 [ "$(sha256 < "$WORK/SHA256SUMS")" = "$RELEASE_CHECKSUMS_SHA256" ] || die "release metadata changed after authenticated preflight"
fi

for name in payesh-install $NEEDED; do
	file="$name-linux-$ARCH.tar.gz"
	say "Downloading $file"
	get_asset "$file" "$WORK/archives/$file"
	expected="$(awk -v f="$file" '$2 == f { print $1; exit }' "$WORK/SHA256SUMS")"
	[ -n "$expected" ] || die "$file is not listed in SHA256SUMS."
	actual="$(sha256 <"$WORK/archives/$file")"
	[ "$actual" = "$expected" ] || die "checksum mismatch for $file; refusing to install."
	tar -xzf "$WORK/archives/$file" -C "$ARTIFACTS"
done
# Hubs retain both Linux architectures for node downloads when GitHub is
# unavailable to a node. These are the core installer/agent artifacts only.
MATRIX="$WORK/matrix"
case "$ROLE" in
standalone | hub)
 mkdir -p "$MATRIX"
 for matrix_arch in amd64 arm64; do
  for matrix_name in payesh-install payesh-agent payesh-privd payesh; do
   if [ "$matrix_arch" = "$ARCH" ]; then
    cp "$ARTIFACTS/$matrix_name" "$MATRIX/$matrix_name-linux-$matrix_arch"
   else
    matrix_archive="$matrix_name-linux-$matrix_arch.tar.gz"
    say "Downloading $matrix_archive"
    get_asset "$matrix_archive" "$WORK/archives/$matrix_archive"
    expected="$(awk -v f="$matrix_archive" '$2 == f { print $1; exit }' "$WORK/SHA256SUMS")"
    [ -n "$expected" ] || die "$matrix_archive is not listed in SHA256SUMS."
    actual="$(sha256 <"$WORK/archives/$matrix_archive")"
    [ "$actual" = "$expected" ] || die "checksum mismatch for $matrix_archive."
    tar -xzOf "$WORK/archives/$matrix_archive" "$matrix_name" >"$MATRIX/$matrix_name-linux-$matrix_arch"
   fi
  done
 done
 ;;
esac
say "All downloads match SHA256SUMS"

# Digest in the same form payesh-install verifies: a plain SHA-256 for a
# binary, and a path-prefixed digest of every file for the web-assets tree.
tree_digest() (
	cd "$1"
	find . -type f | sed 's|^\./||' | LC_ALL=C sort | while IFS= read -r f; do
		printf 'path\000%s\000' "$f"
		cat "$f"
	done | sha256
)

set --
for name in $NEEDED; do
	if [ "$name" = web-assets ]; then
		digest="$(tree_digest "$ARTIFACTS/web-assets")"
	else
		digest="$(sha256 <"$ARTIFACTS/$name")"
	fi
	set -- "$@" --artifact-sha256 "$name=$digest"
done
[ -z "$LISTEN" ] || set -- "$@" --listen "$LISTEN"

INSTALLER="$ARTIFACTS/payesh-install"
chmod 0755 "$INSTALLER"

set -- --role "$ROLE" "$@"
if [ -n "$CONVERT_FROM" ]; then
	set -- "$@" --convert-from "$CONVERT_FROM" --transport-url "$TRANSPORT_URL" --node-identity-file "$NODE_IDENTITY_FILE" --hub-ca-file "$HUB_CA_FILE"
fi

# --- install ---------------------------------------------------------------

# Old signed installers without this read-only capability fail closed.
# The candidate's own compiled registry is checked before any installed paths
# (including the architecture matrix) or services are changed.
if [ "$RELEASE_MODE" = production ]; then
 "$INSTALLER" --check-schema || die "candidate database schema preflight failed or installer lacks support"
fi

if ! "$INSTALLER" "$@" >"$WORK/preflight"; then
	cat "$WORK/preflight" >&2
	die "this server did not pass the Payesh preflight (see the problems above)."
fi
if [ "$CHECK_ONLY" = 1 ]; then
	say "Preflight passed. Nothing was installed (--check)."
	exit 0
fi

if [ -d "$MATRIX" ]; then
 mkdir -p /usr/share/payesh/matrix
 chmod 0755 /usr/share/payesh /usr/share/payesh/matrix
 for matrix_file in "$MATRIX"/*; do
  matrix_target="/usr/share/payesh/matrix/${matrix_file##*/}"
  cp "$matrix_file" "$matrix_target.tmp"
  chmod 0755 "$matrix_target.tmp"
  mv -f "$matrix_target.tmp" "$matrix_target"
 done
fi
"$INSTALLER" --install --start --artifact-dir "$ARTIFACTS" "$@" >"$WORK/result.json"
say "Payesh $VERSION $DONE."
case "$ROLE" in
standalone | hub) ;;
*) exit 0 ;;
esac

PORT="${LISTEN##*:}"
PORT="${PORT:-8787}"
IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }')"
IP="${IP:-SERVER_IP}"

if [ -n "$DOMAIN" ]; then
 set -- "$DOMAIN"
 [ -z "$EMAIL" ] || set -- "$@" --email "$EMAIL"
 /usr/bin/payesh domain "$@" >"$WORK/domain-result" || die "HTTPS setup failed."
fi

# Read saved certificate state rather than assuming HTTP on every rerun.
STATUS="$(/usr/bin/payesh domain 2>/dev/null)" || die "could not read HTTPS settings."
SAVED_DOMAIN="$(printf '%s\n' "$STATUS" | awk '$1 == "domain:" {print $2}')"
TLS_STATE="$(printf '%s\n' "$STATUS" | awk '$1 == "state:" {print $2}')"
for service in /etc/systemd/system/payesh-server.service /etc/init.d/payesh-server; do
 if [ -r "$service" ]; then
  SAVED_LISTEN="$(tr '\042\047' '  ' <"$service" | sed -n 's/.*-listen=\([^ ]*\).*/\1/p' | head -n 1)"
  [ -z "$SAVED_LISTEN" ] || PORT="${SAVED_LISTEN##*:}"
  break
 fi
done
if [ "$TLS_STATE" = active ]; then
 printf 'Dashboard: https://%s:%s\n' "$SAVED_DOMAIN" "$PORT"
else
 printf 'Dashboard: http://%s:%s\n' "$IP" "$PORT"
 [ -z "$SAVED_DOMAIN" ] || warn "HTTPS for $SAVED_DOMAIN is $TLS_STATE."
fi
if [ "$UPDATING" = 0 ] && [ -r /etc/payesh/owner-credentials ]; then
 sed 's/^/  /' /etc/payesh/owner-credentials
fi
