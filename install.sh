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

usage() {
    cat <<'EOF'
Usage: install.sh [options]

Options:
  --check-only        Run preflight only; change nothing, exit non-zero on failure
  --yes               Non-interactive mode (assume yes for prompts)
  --from-local PATH   Install the given locally built tinyship binary
  -h, --help          Show this help

Supported hosts: Ubuntu 22.04/24.04, Debian 12, amd64,
Docker Engine >= 24.0 (API >= 1.43).
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

 # 4. Ports 80/443 (Caddy needs them)
    local port
    for port in 80 443; do
        if listening_on "$port"; then
            fail "port ${port} is already in use; Caddy cannot start (stop the service using it or reassign it)"
            ok=1
        else
            log "preflight: port ${port} free"
        fi
    done

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

install_from_local() {
    local src=$1
    [[ -x "$src" ]] || { fail "--from-local path is not an executable: $src"; return 1; }

    if [[ $EUID -ne 0 ]]; then
        fail "install requires root (re-run with sudo); preflight does not"
        return 1
    fi

 # Idempotent: never overwrite existing config, data, or keys.
    id tinyship >/dev/null 2>&1 || useradd --system --home /var/lib/tinyship --shell /usr/sbin/nologin tinyship
    id tinyship-worker >/dev/null 2>&1 || useradd --system --home /var/lib/tinyship --shell /usr/sbin/nologin tinyship-worker
    getent group tinyship >/dev/null 2>&1 || groupadd --system tinyship
    getent group tinyship-db >/dev/null 2>&1 || groupadd --system tinyship-db
    getent group tinyship-admin >/dev/null 2>&1 || groupadd --system tinyship-admin

    install -d -m 0755 /usr/local/bin
    if [[ -x /usr/local/bin/tinyship ]]; then
        log "existing /usr/local/bin/tinyship kept (idempotent install)"
    else
        install -m 0755 "$src" /usr/local/bin/tinyship
        log "installed /usr/local/bin/tinyship"
    fi

    install -d -o tinyship -g tinyship-db -m 0770 /var/lib/tinyship
    install -d -o root -g tinyship -m 0640 /etc/tinyship 2>/dev/null || true

    log "systemd units, Caddy baseline, and keys arrive later; not installed by this revision"
    log "install done; run: tinyship version"
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
