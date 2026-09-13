#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
Usage: configure-public-access.sh [--domain NAME] [--upstream HOST:PORT] [--email ADDRESS] [--apply]

Without --apply this command prints the planned configuration. When no domain
is supplied, Payesh remains loopback-only and the command prints an SSH tunnel
command. Domain mode installs/configures Caddy and obtains/renews TLS
automatically. Existing listeners on ports 80 or 443 are never stopped.
EOF
}

domain=
upstream=127.0.0.1:8787
email=
apply=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --domain) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; domain=$2; shift 2 ;;
    --upstream) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; upstream=$2; shift 2 ;;
    --email) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; email=$2; shift 2 ;;
    --apply) apply=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$upstream" in
  127.0.0.1:*) upstream_port=${upstream#127.0.0.1:} ;;
  localhost:*) upstream_port=${upstream#localhost:} ;;
  '[::1]':*) upstream_port=${upstream#'[::1]':} ;;
  *) printf 'refusing non-loopback upstream: %s\n' "$upstream" >&2; exit 2 ;;
esac
case "$upstream_port" in
  ''|*[!0-9]*) printf 'invalid upstream port: %s\n' "$upstream_port" >&2; exit 2 ;;
esac
if [ "$upstream_port" -lt 1 ] || [ "$upstream_port" -gt 65535 ]; then
  printf 'upstream port is outside 1-65535: %s\n' "$upstream_port" >&2
  exit 2
fi
if [ -n "$email" ]; then
  case "$email" in
    *@*.*) ;;
    *) printf 'invalid ACME contact email\n' >&2; exit 2 ;;
  esac
  case "$email" in
    *[!A-Za-z0-9._%+-@]*) printf 'invalid ACME contact email\n' >&2; exit 2 ;;
  esac
fi

if [ -z "$domain" ]; then
  cat >&2 <<EOF
WARNING: no domain was supplied, so publicly trusted browser TLS cannot be configured.
Payesh will remain loopback-only. Use this encrypted SSH tunnel:
  ssh -N -L 8787:${upstream} root@YOUR_SERVER_IP
Then open http://127.0.0.1:8787 locally. Add a domain later for unattended HTTPS access.
EOF
  exit 0
fi

case "$domain" in
  *[!A-Za-z0-9.-]*|.*|*..*|*.) printf 'invalid domain: %s\n' "$domain" >&2; exit 2 ;;
esac

if ! command -v getent >/dev/null 2>&1; then
  printf 'getent is required to verify DNS\n' >&2
  exit 1
fi
resolved=$(getent ahostsv4 "$domain" 2>/dev/null | awk 'NR == 1 { print $1 }')
[ -n "$resolved" ] || { printf 'domain does not resolve to an IPv4 address: %s\n' "$domain" >&2; exit 1; }
public_ip=$(curl -4fsS --max-time 10 https://api.ipify.org 2>/dev/null || true)
if [ -n "$public_ip" ] && [ "$resolved" != "$public_ip" ]; then
  printf 'warning: domain %s resolves to %s while outbound IPv4 is %s; continuing because NAT or asymmetric routing may be intentional\n' "$domain" "$resolved" "$public_ip" >&2
fi

managed_config=false
if [ -f /etc/caddy/Caddyfile ] && grep -q '^# Managed by Payesh configure-public-access$' /etc/caddy/Caddyfile; then
  managed_config=true
fi
if [ -s /etc/caddy/Caddyfile ] && [ "$managed_config" != true ]; then
  printf 'an existing Caddy configuration is not owned by Payesh; refusing to overwrite it\n' >&2
  exit 1
fi
for port in 80 443; do
  if ss -H -ltn "sport = :$port" 2>/dev/null | grep -q .; then
    if [ "$managed_config" != true ] || ! ss -H -ltnp "sport = :$port" 2>/dev/null | grep -q 'caddy'; then
      printf 'port %s is already in use; refusing to disrupt the existing listener\n' "$port" >&2
      exit 1
    fi
  fi
done

printf 'domain=%s upstream=%s dns=%s mode=automatic-https\n' "$domain" "$upstream" "$resolved"
[ "$apply" = true ] || { printf 'dry-run only; pass --apply to install and configure Caddy\n'; exit 0; }
[ "$(id -u)" -eq 0 ] || { printf -- '--apply must run as root\n' >&2; exit 1; }

tmp=
key_tmp=
list_tmp=
env_tmp=
trap 'for file in "$tmp" "$key_tmp" "$list_tmp" "$env_tmp"; do [ -z "$file" ] || rm -f -- "$file"; done' EXIT HUP INT TERM

if ! command -v caddy >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y ca-certificates curl debian-keyring debian-archive-keyring apt-transport-https gnupg
    if ! apt-cache show caddy >/dev/null 2>&1; then
      key_tmp=$(mktemp)
      list_tmp=$(mktemp)
      curl -1fsSL --proto '=https' --tlsv1.2 https://dl.cloudsmith.io/public/caddy/stable/gpg.key -o "$key_tmp"
      gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg "$key_tmp"
      curl -1fsSL --proto '=https' --tlsv1.2 https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt -o "$list_tmp"
      install -m 0644 "$list_tmp" /etc/apt/sources.list.d/caddy-stable.list
      chmod o+r /usr/share/keyrings/caddy-stable-archive-keyring.gpg
      apt-get update
    fi
    apt-get install -y caddy
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache caddy ca-certificates curl
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y caddy ca-certificates curl
  else
    printf 'unsupported package manager; install Caddy and retry\n' >&2
    exit 1
  fi
fi

tmp=$(mktemp)
{
  printf '# Managed by Payesh configure-public-access\n'
  [ -z "$email" ] || printf '{\n\temail %s\n}\n\n' "$email"
  printf '%s {\n' "$domain"
  printf '\tencode zstd gzip\n'
  # Keep the API private behind the proxy while serving the installed SPA.
  # The server intentionally protects non-health API routes with auth; sending
  # the root document through reverse_proxy therefore produced a JSON 401 and
  # made the browser UI unreachable.
  printf '\troot * /usr/share/payesh/web-assets\n'
  printf '\t@backend path /api/* /healthz\n'
  printf '\thandle @backend {\n'
  printf '\t\treverse_proxy %s\n' "$upstream"
  printf '\t}\n'
  printf '\thandle {\n'
  printf '\t\ttry_files {path} /index.html\n'
  printf '\t\tfile_server\n'
  printf '\t}\n'
  printf '}\n'
} >"$tmp"
caddy validate --config "$tmp" --adapter caddyfile
install -m 0644 "$tmp" /etc/caddy/Caddyfile
if [ -d /etc/payesh ]; then
  env_file=/etc/payesh/payesh.env
  env_tmp=$(mktemp)
  if [ -f "$env_file" ]; then
    grep -v '^PAYESH_SECURE_BROWSER_COOKIES=' "$env_file" >"$env_tmp" || true
  fi
  printf 'PAYESH_SECURE_BROWSER_COOKIES=true\n' >>"$env_tmp"
  install -m 0640 "$env_tmp" "$env_file"
fi
if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now caddy
  systemctl reload caddy
elif command -v rc-service >/dev/null 2>&1; then
  rc-update add caddy default
  rc-service caddy restart
else
  printf 'Caddy was configured but no supported service manager was found\n' >&2
  exit 1
fi
printf 'automatic HTTPS configured: https://%s\n' "$domain"
