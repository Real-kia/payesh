#!/bin/sh
# Payesh one-line installer.
#
#   curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh
#
# Downloads the Payesh release for this machine from GitHub Releases, checks
# every archive against the release SHA256SUMS, and hands the verified files
# to payesh-install, which creates the services and starts them.
#
# Options (flags, or the matching environment variable):
#   --role ROLE        standalone (default), hub, node, or cli-only   PAYESH_ROLE
#   --version X.Y.Z    release to install (default: latest)          PAYESH_VERSION
#   --listen ADDR      web listen address (default 0.0.0.0:8787)     PAYESH_LISTEN
#   --domain NAME      get a free HTTPS certificate for NAME          PAYESH_DOMAIN
#   --email ADDR       optional Let's Encrypt contact email           PAYESH_EMAIL
#   --check            only run the host preflight, change nothing
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

REPO="${PAYESH_REPO:-Real-kia/payesh}"
ROLE="${PAYESH_ROLE:-standalone}"
VERSION="${PAYESH_VERSION:-latest}"
LISTEN="${PAYESH_LISTEN:-}"
DOMAIN="${PAYESH_DOMAIN:-}"
EMAIL="${PAYESH_EMAIL:-}"
CHECK_ONLY=0
TOKEN="${GITHUB_TOKEN:-}"

say() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--role) ROLE="${2:?--role needs a value}"; shift 2 ;;
	--role=*) ROLE="${1#*=}"; shift ;;
	--version) VERSION="${2:?--version needs a value}"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--listen) LISTEN="${2:?--listen needs a value}"; shift 2 ;;
	--listen=*) LISTEN="${1#*=}"; shift ;;
	--domain) DOMAIN="${2:?--domain needs a value}"; shift 2 ;;
	--domain=*) DOMAIN="${1#*=}"; shift ;;
	--email) EMAIL="${2:?--email needs a value}"; shift 2 ;;
	--email=*) EMAIL="${1#*=}"; shift ;;
	--check) CHECK_ONLY=1; shift ;;
	-h | --help)
		echo "usage: install.sh [--role standalone|hub|node|cli-only] [--version X.Y.Z] [--listen ADDR] [--domain NAME] [--email ADDR] [--check]"
		exit 0
		;;
	*) die "unknown option: $1 (see --help)" ;;
	esac
done

case "$ROLE" in
standalone | hub | node | cli-only) ;;
*) die "unsupported role '$ROLE' (use standalone, hub, node, or cli-only)" ;;
esac
VERSION="${VERSION#v}"

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

if [ -n "$TOKEN" ]; then
	warn "GITHUB_TOKEN is set: downloading through the GitHub API (private repository mode)."
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

say "Installing Payesh ${VERSION:-latest} ($ROLE, linux/$ARCH) from github.com/$REPO"
get_asset SHA256SUMS "$WORK/SHA256SUMS"

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

# --- install ---------------------------------------------------------------

say "Checking this server"
if ! "$INSTALLER" --role "$ROLE" ${LISTEN:+--listen "$LISTEN"}; then
	die "this server did not pass the Payesh preflight (see the problems above)."
fi
if [ "$CHECK_ONLY" = 1 ]; then
	say "Preflight passed. Nothing was installed (--check)."
	exit 0
fi

# Earlier installs forced Secure cookies, which browsers refuse over plain
# HTTP. Payesh still marks cookies Secure automatically on HTTPS requests.
ENV_FILE=/etc/payesh/payesh.env
if [ -f "$ENV_FILE" ] && grep -q '^PAYESH_SECURE_BROWSER_COOKIES=true$' "$ENV_FILE" && ! grep -q '^PAYESH_ALLOW_INSECURE_HTTP=' "$ENV_FILE"; then
	sed -i 's/^PAYESH_SECURE_BROWSER_COOKIES=true$/PAYESH_ALLOW_INSECURE_HTTP=true/' "$ENV_FILE"
	say "Updated $ENV_FILE for public HTTP access"
fi

say "Installing and starting services"
"$INSTALLER" --role "$ROLE" --install --start --artifact-dir "$ARTIFACTS" "$@"

echo
say "Payesh $VERSION is installed."
case "$ROLE" in
standalone | hub) ;;
*) exit 0 ;;
esac

PORT="${LISTEN##*:}"
PORT="${PORT:-8787}"
IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }')"
IP="${IP:-SERVER_IP}"

HTTPS_OK=0
if [ -n "$DOMAIN" ]; then
	say "Setting up HTTPS for $DOMAIN"
	if /usr/bin/payesh domain "$DOMAIN" ${EMAIL:+--email "$EMAIL"}; then
		HTTPS_OK=1
	else
		warn "HTTPS setup did not finish; Payesh is still reachable over HTTP. Fix the problem above, then run: sudo payesh domain $DOMAIN"
	fi
fi

echo
if [ "$HTTPS_OK" = 1 ]; then
	printf '  Dashboard:  https://%s:%s\n' "$DOMAIN" "$PORT"
else
	printf '  Dashboard:  http://%s:%s\n' "$IP" "$PORT"
fi
if [ -r /etc/payesh/owner-credentials ]; then
	sed 's/^/  /' /etc/payesh/owner-credentials
	echo "  (saved in /etc/payesh/owner-credentials)"
fi
if [ "$HTTPS_OK" != 1 ]; then
	printf '\n  \033[1;33mWARNING: no SSL.\033[0m The connection is not encrypted, so your password\n'
	cat <<MSG
  and data travel in plain text. To turn on HTTPS, point a domain or
  subdomain at $IP and run:

      sudo payesh domain panel.example.com

  Payesh gets a free Let's Encrypt certificate and renews it automatically.
  HTTPS runs on port $PORT; port 443 and nginx are not touched.
MSG
fi
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
	printf '\n  Firewall (ufw) is active. Allow the dashboard with: sudo ufw allow %s/tcp\n' "$PORT"
elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
	printf '\n  firewalld is active. Allow the dashboard with:\n      sudo firewall-cmd --permanent --add-port=%s/tcp && sudo firewall-cmd --reload\n' "$PORT"
fi
echo
