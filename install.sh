#!/usr/bin/env bash
# TinyShip installer with preflight.
#
# Scope of this revision:
# - preflight is complete and non-destructive: supported OS,
# available RAM, free disk, ports 80/443, Docker presence/version;
# `--check-only` runs preflight and exits without changing anything.
# - install supports `--from-local <path-to-binary>` (dev/self-host path).
# The download-from-release path (checksum verification)
# activates once releases exist; until then the script refuses to guess
# a download URL.
#
# Provisional thresholds: every number here must be re-tuned by
# the benchmark before release.
set -euo pipefail

MIN_RAM_MB=1024       # provisional minimum available RAM
MIN_DISK_MB=5120      # provisional minimum free disk on /
DOCKER_MIN_API=1.43   # Docker Engine 24.0

CHECK_ONLY=0
ASSUME_YES=0
FROM_LOCAL=""
# TINYSHIP_INSTALL_ROOT prefixes every path the installer writes. It
# exists for development and test hosts: a sandbox install writes files
# only, touching no users, groups, or services, and needs no root.
INSTALL_ROOT="${TINYSHIP_INSTALL_ROOT:-}"

sandbox() { [[ -n "$INSTALL_ROOT" ]]; }

# resolve_caddy prints the caddy binary path when one is available.
resolve_caddy() {
    if [[ -n "${TINYSHIP_CADDY_BIN:-}" && -x "${TINYSHIP_CADDY_BIN:-}" ]]; then
        printf '%s\n' "$TINYSHIP_CADDY_BIN"
    else
        command -v caddy 2>/dev/null || true
    fi
}

usage() {
    cat <<'EOF'
Usage: install.sh [options]

Options:
  --check-only        Run preflight only; change nothing, exit non-zero on failure
  --yes               Non-interactive mode (assume yes for prompts)
  --from-local PATH   Install the given locally built tinyship binary
  -h, --help          Show this help

Environment:
  TINYSHIP_INSTALL_ROOT  Write under this prefix instead of / (dev/test only;
                         skips users, groups, services, and the port checks)

Supported hosts: Ubuntu 22.04/24.04, Debian 12, amd64,
Docker Engine >= 24.0 (API >= 1.43), Caddy >= 2.0.
EOF
}

log()  { printf '[tinyship] %s\n' "$*"; }
fail() { printf '[tinyship] FAIL: %s\n' "$*" >&2; }
warn() { printf '[tinyship] warn: %s\n' "$*"; }

# ---------------------------------------------------------------- preflight

preflight() {
    local ok=0

    log "preflight: checking host (no changes are made)"

 # 1. OS
    if [[ -r /etc/os-release ]]; then
 # shellcheck disable=SC1091
        . /etc/os-release
        case "${ID:-}:${VERSION_ID:-}" in
            ubuntu:22.04|ubuntu:24.04|debian:12)
                log "preflight: OS ${PRETTY_NAME:-$ID $VERSION_ID} supported" ;;
            *)
                fail "unsupported OS: ${PRETTY_NAME:-$ID $VERSION_ID} (need Ubuntu 22.04/24.04 or Debian 12)"
                ok=1 ;;
        esac
        if [[ "$(uname -m)" != "x86_64" ]]; then
            fail "unsupported architecture: $(uname -m) (need amd64 in v0.1)"
            ok=1
        fi
    else
        fail "cannot read /etc/os-release; cannot verify OS support"
        ok=1
    fi

 # 2. RAM available (not total)
    local ram_mb
    ram_mb=$(awk '/^MemAvailable:/ {printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || echo 0)
    if (( ram_mb < MIN_RAM_MB )); then
        fail "available RAM ${ram_mb} MiB is below the ${MIN_RAM_MB} MiB minimum"
        ok=1
    else
        log "preflight: available RAM ${ram_mb} MiB (>= ${MIN_RAM_MB})"
    fi

 # 3. Free disk on /
    local disk_mb
    disk_mb=$(df -Pm / | awk 'NR==2 {print $4}')
    if (( disk_mb < MIN_DISK_MB )); then
        fail "free disk on / is ${disk_mb} MiB, below the ${MIN_DISK_MB} MiB minimum"
        ok=1
    else
        log "preflight: free disk ${disk_mb} MiB (>= ${MIN_DISK_MB})"
    fi

 # 4. Ports 80/443 (Caddy needs them). A sandbox install root starts
 # no services, so nothing can conflict with them.
    if sandbox; then
        log "preflight: sandbox install root set; port checks skipped (no services are started)"
    else
        local port
        for port in 80 443; do
            if listening_on "$port"; then
                fail "port ${port} is already in use; Caddy cannot start (stop the service using it or reassign it)"
                ok=1
            else
                log "preflight: port ${port} free"
            fi
        done
    fi

 # 5. Docker
    if ! command -v docker >/dev/null 2>&1; then
        warn "Docker is not installed; the installer will ask before installing it"
    else
        local api_version
        api_version=$(docker version --format '{{.Server.APIVersion}}' 2>/dev/null || echo "unknown")
        if [[ "$api_version" == "unknown" ]]; then
            fail "Docker is installed but the daemon is unreachable (try: systemctl start docker)"
            ok=1
        elif printf '%s\n%s\n' "$DOCKER_MIN_API" "$api_version" | sort -V -C; then
            log "preflight: Docker API ${api_version} (>= ${DOCKER_MIN_API})"
        else
            fail "Docker API ${api_version} is older than the required ${DOCKER_MIN_API} (Engine >= 24.0)"
            ok=1
        fi
    fi

 # 6. Caddy (the proxy that serves domains over HTTPS)
    local caddy_bin
    caddy_bin=$(resolve_caddy)
    if [[ -z "$caddy_bin" ]]; then
        warn "Caddy is not installed; the installer will skip the proxy wiring (deploys work, domains cannot route)"
    else
        log "preflight: Caddy $("$caddy_bin" version 2>/dev/null | awk '{print $1}') available"
    fi

    return "$ok"
}

listening_on() {
    local port=$1
    if command -v ss >/dev/null 2>&1; then
        ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "(:|\\*)${port}$"
    else
 # Fallback: parse /proc/net/tcp{,6}; local_address is hex.
        local hex
        hex=$(printf '%04X' "$port")
        grep -qE "^[[:space:]]*[0-9]+: [0-9A-F]{8,32}:${hex} " /proc/net/tcp /proc/net/tcp6 2>/dev/null
    fi
}

# ------------------------------------------------------------------ install

# write_caddy_baseline installs the base proxy config: admin over the
# unix socket, automatic HTTPS at Caddy's default (enabled), and an
# empty tinyship server that the worker replaces wholesale at deploy
# time. An existing file is never overwritten.
write_caddy_baseline() {
    local path="$INSTALL_ROOT/etc/caddy/caddy.json"
    if [[ -e "$path" ]]; then
        log "existing $path kept (idempotent install)"
        return 0
    fi
    install -d -m 0755 "$(dirname "$path")"
    cat >"$path" <<'EOF'
{
  "admin": {"listen": "unix//run/caddy/admin.sock|0660"},
  "apps": {"http": {"servers": {"tinyship": {}}}}
}
EOF
    chmod 0640 "$path"
    if ! sandbox && getent group caddy >/dev/null 2>&1; then
        chown root:caddy "$path"
    fi
    log "installed $path (proxy baseline; only the tinyship server subtree is ever replaced)"
}

# write_caddy_unit installs the systemd drop-in that points Caddy at
# the TinyShip baseline. The service group makes the admin socket group
# caddy-admin, which tinyship-worker joins; RuntimeDirectory creates
# /run/caddy. An existing file is never overwritten.
write_caddy_unit() {
    local caddy_bin dropin
    caddy_bin=$(resolve_caddy)
    dropin="$INSTALL_ROOT/etc/systemd/system/caddy.service.d/tinyship.conf"
    if [[ -z "$caddy_bin" ]]; then
        warn "Caddy binary not found; skipping $dropin (install Caddy, then re-run the installer)"
        return 0
    fi
    if [[ -e "$dropin" ]]; then
        log "existing $dropin kept (idempotent install)"
        return 0
    fi
    install -d -m 0755 "$(dirname "$dropin")"
    cat >"$dropin" <<EOF
[Service]
Group=caddy-admin
RuntimeDirectory=caddy
RuntimeDirectoryMode=0755
ExecStart=
ExecStart=$caddy_bin run --config /etc/caddy/caddy.json
EOF
    chmod 0644 "$dropin"
    log "installed $dropin (reload with: systemctl daemon-reload && systemctl restart caddy)"
}

install_from_local() {
    local src=$1
    [[ -x "$src" ]] || { fail "--from-local path is not an executable: $src"; return 1; }

    if sandbox; then
        log "sandbox mode: writing under $INSTALL_ROOT only (no users, groups, or services are touched)"
    elif [[ $EUID -ne 0 ]]; then
        fail "install requires root (re-run with sudo); preflight does not"
        return 1
    fi

 # Idempotent: never overwrite existing config, data, or keys.
    if ! sandbox; then
        id tinyship >/dev/null 2>&1 || useradd --system --home /var/lib/tinyship --shell /usr/sbin/nologin tinyship
        id tinyship-worker >/dev/null 2>&1 || useradd --system --home /var/lib/tinyship --shell /usr/sbin/nologin tinyship-worker
        getent group tinyship >/dev/null 2>&1 || groupadd --system tinyship
        getent group tinyship-db >/dev/null 2>&1 || groupadd --system tinyship-db
        getent group tinyship-admin >/dev/null 2>&1 || groupadd --system tinyship-admin
        # The worker reaches the proxy admin socket through this group.
        getent group caddy-admin >/dev/null 2>&1 || groupadd --system caddy-admin
        usermod -a -G caddy-admin tinyship-worker
    fi

    install -d -m 0755 "$INSTALL_ROOT/usr/local/bin"
    if [[ -x "$INSTALL_ROOT/usr/local/bin/tinyship" ]]; then
        log "existing $INSTALL_ROOT/usr/local/bin/tinyship kept (idempotent install)"
    else
        install -m 0755 "$src" "$INSTALL_ROOT/usr/local/bin/tinyship"
        log "installed $INSTALL_ROOT/usr/local/bin/tinyship"
    fi

    if sandbox; then
        install -d -m 0750 "$INSTALL_ROOT/var/lib/tinyship"
        install -d -m 0750 "$INSTALL_ROOT/etc/tinyship"
    else
        install -d -o tinyship -g tinyship-db -m 0770 "$INSTALL_ROOT/var/lib/tinyship"
        install -d -o root -g tinyship -m 0640 "$INSTALL_ROOT/etc/tinyship" 2>/dev/null || true
    fi

    write_caddy_baseline
    write_caddy_unit

    bootstrap_token

    log "TinyShip's own systemd units and keys arrive later; not installed by this revision"
    log "install done; run: tinyship version"
}

# bootstrap_token issues the first admin token: it runs the database
# migrations (creating the DB if absent) and writes the plaintext token to
# /etc/tinyship/token (mode 0640). The value is printed once by the token
# command; here it is suppressed so the log does not carry the credential.
# A re-run is idempotent: an existing token file is never overwritten.
bootstrap_token() {
    local bin="$INSTALL_ROOT/usr/local/bin/tinyship"
    local db="$INSTALL_ROOT/var/lib/tinyship/tinyship.db"
    local tf="$INSTALL_ROOT/etc/tinyship/token"

    if [[ -e "$tf" ]]; then
        log "existing $tf kept (idempotent install)"
        return 0
    fi

    log "issuing the first admin token (hash stored; plaintext written to $tf and shown once)"
    if ! TINYSHIP_DB="$db" TINYSHIP_TOKEN_FILE="$tf" "$bin" token create >/dev/null 2>"$INSTALL_ROOT/token.err"; then
        cat "$INSTALL_ROOT/token.err" >&2
        rm -f "$INSTALL_ROOT/token.err"
        fail "could not issue the admin token; run 'tinyship token create' after fixing the error"
        return 1
    fi
    rm -f "$INSTALL_ROOT/token.err"
    if ! sandbox; then
        # The token command runs as root, so re-own the files it created:
        # the DB must be writable by the services, the token readable by
        # the admin group.
        chown tinyship:tinyship-db "$db" "$db-wal" "$db-shm" 2>/dev/null || true
        chown root:tinyship-admin "$tf" 2>/dev/null || true
    fi
    log "admin token created; keep the plaintext private (it is not recoverable from the database)"
}

# --------------------------------------------------------------------- main

while [[ $# -gt 0 ]]; do
    case "$1" in
        --check-only) CHECK_ONLY=1 ;;
        --yes)        ASSUME_YES=1 ;;
        --from-local) shift; FROM_LOCAL="${1:-}"; [[ -n "$FROM_LOCAL" ]] || { fail "--from-local needs a path"; exit 2; } ;;
        -h|--help)    usage; exit 0 ;;
        *)            fail "unknown option: $1"; usage >&2; exit 2 ;;
    esac
    shift
done

if preflight; then
    log "preflight: PASS"
else
    fail "preflight: FAIL — fix the items above and re-run"
    exit 1
fi

if (( CHECK_ONLY )); then
    log "--check-only: exiting without changes"
    exit 0
fi

if [[ -z "$FROM_LOCAL" ]]; then
    fail "no install method given; this pre-release revision supports --from-local PATH (release downloads are not available yet)"
    exit 2
fi

if ! command -v docker >/dev/null 2>&1 && (( ! ASSUME_YES )); then
    fail "Docker is not installed; re-run with --yes to acknowledge, or install Docker >= 24.0 yourself"
    exit 1
fi

install_from_local "$FROM_LOCAL"
