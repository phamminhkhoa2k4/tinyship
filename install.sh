#!/usr/bin/env bash
# TinyShip installer with preflight.
#
# Scope of this revision:
# - preflight is complete and non-destructive: supported OS,
# available RAM, free disk, ports 80/443, Docker presence/version;
# `--check-only` runs preflight and exits without changing anything.
# - install supports `--from-local <path-to-binary>` (dev/self-host path)
# and wires a working host: Docker Engine through apt when missing, the
# two hardened systemd services (started now, restarted on boot), the
# sealed-secret key pair, the host config file, the proxy baseline, and
# the first admin token.
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
  --yes               Non-interactive mode (assume yes for prompts, including
                      the Docker install)
  --from-local PATH   Install the given locally built tinyship binary
  -h, --help          Show this help

Environment:
  TINYSHIP_INSTALL_ROOT  Write under this prefix instead of / (dev/test only;
                         skips users, groups, services, and the port checks)

What a full install does: preflight, Docker Engine via apt when missing
(prompted unless --yes), the two systemd services with hardening and
autostart on boot, the sealed-secret key pair, the host config file, the
proxy baseline (when Caddy is present), the first admin token (shown
once), then starts and health-checks the services.

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

 # 4. Ports 80/443 exist for the proxy. A sandbox install root starts
 # no services, so nothing can conflict with them; without Caddy this
 # install wires no proxy either, so a busy port is then a warning
 # (it only blocks a later Caddy setup), not a failure.
    if sandbox; then
        log "preflight: sandbox install root set; port checks skipped (no services are started)"
    else
        local port caddy_here
        caddy_here=0
        [[ -n "$(resolve_caddy)" ]] && caddy_here=1
        for port in 80 443; do
            if listening_on "$port"; then
                if (( caddy_here )); then
                    fail "port ${port} is already in use; Caddy cannot start (stop the service using it or reassign it)"
                    ok=1
                else
                    warn "port ${port} is in use; no proxy runs in this install (Caddy absent), this only blocks a later Caddy setup"
                fi
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

# confirm asks a yes/no question. --yes answers yes so non-interactive
# runs work; a closed stdin or any other answer is no.
confirm() {
    if (( ASSUME_YES )); then
        log "$1 yes (--yes)"
        return 0
    fi
    if [[ ! -t 0 ]]; then
        fail "$1 non-interactive run: re-run with --yes to approve"
        return 1
    fi
    local answer
    read -r -p "$1 [y/N] " answer
    [[ "$answer" == "y" || "$answer" == "Y" || "$answer" == "yes" ]]
}

# ensure_docker installs Docker Engine from the vendor's apt repository
# when it is missing: apt prerequisites, the vendor signing key, the
# package repository, the engine packages, then the daemon. Sandbox
# installs never touch host packages. Interactive runs confirm first;
# non-interactive runs need --yes.
ensure_docker() {
    if sandbox; then
        warn "sandbox mode: the host must provide Docker; not installing packages"
        return 0
    fi
    log "Docker is not installed"
    if ! confirm "Install Docker Engine from Docker's official apt repository now?"; then
        fail "Docker is required; install Docker >= 24.0 yourself and re-run the installer"
        return 1
    fi

    log "installing Docker Engine (apt)"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq \
        || { fail "apt-get update failed; cannot install Docker"; return 1; }
    apt-get install -y -qq ca-certificates curl \
        || { fail "could not install apt prerequisites (ca-certificates, curl)"; return 1; }
    install -d -m 0755 /etc/apt/keyrings
    curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc \
        || { fail "could not download Docker's repository signing key"; return 1; }
    chmod a+r /etc/apt/keyrings/docker.asc
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${ID} ${VERSION_CODENAME} stable" \
        > /etc/apt/sources.list.d/docker.list \
        || { fail "could not add Docker's apt repository"; return 1; }
    apt-get update -qq \
        || { fail "apt-get update with the Docker repository failed"; return 1; }
    apt-get install -y -qq docker-ce docker-ce-cli containerd.io \
        || { fail "Docker installation failed; install Docker >= 24.0 manually and re-run"; return 1; }
    systemctl enable --now docker \
        || { fail "Docker was installed but its service could not start (try: systemctl start docker)"; return 1; }

    local api_version
    api_version=$(docker version --format '{{.Server.APIVersion}}' 2>/dev/null || echo "unknown")
    if [[ "$api_version" == "unknown" ]] \
        || ! printf '%s\n%s\n' "$DOCKER_MIN_API" "$api_version" | sort -V -C; then
        fail "Docker API ${api_version} does not meet the ${DOCKER_MIN_API} requirement after install"
        return 1
    fi
    log "Docker Engine installed (API ${api_version})"
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
        # The worker drives containers through the Docker socket; on this
        # platform membership in docker is equivalent to root (documented
        # in the threat model, and the reason the worker is a separate
        # user from the API).
        if getent group docker >/dev/null 2>&1; then
            usermod -a -G docker tinyship-worker
        fi
        # Whoever invoked sudo gets the admin group so the CLI can read
        # the token file and reach the API socket afterwards.
        if [[ -n "${SUDO_USER:-}" ]] && id "$SUDO_USER" >/dev/null 2>&1; then
            usermod -a -G tinyship-admin "$SUDO_USER"
            log "added $SUDO_USER to tinyship-admin (log out and back in for it to take effect)"
        fi
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
        # The setgid bit keeps SQLite's write-ahead log in the shared
        # group even when the API or the worker is the process that
        # creates it; both units run with UMask=0007 so such files land
        # group-writable (0660) for the other service.
        install -d -o tinyship -g tinyship-db -m 2770 "$INSTALL_ROOT/var/lib/tinyship"
        # 0751: root writes, the services read via the tinyship group,
        # and admin-group members can traverse to the token file without
        # being able to list the directory.
        install -d -o root -g tinyship -m 0751 "$INSTALL_ROOT/etc/tinyship"
    fi

    write_keys
    write_config
    write_caddy_baseline
    write_caddy_unit
    write_tmpfiles
    write_units

    bootstrap_token

    start_services

    log "install done; the admin token above was shown once — store it safely"
    log "create your first app: tinyship app create --name web --image traefik/whoami --port 8080"
}

# write_keys generates the sealed-secret key pair on first install. The
# pair is never regenerated: stored secrets encrypted for an old key
# would become unreadable, so an existing pair is always kept.
write_keys() {
    local bin="$INSTALL_ROOT/usr/local/bin/tinyship"
    local dir="$INSTALL_ROOT/etc/tinyship/keys"

    if [[ -e "$dir/secrets.key" || -e "$dir/secrets.pub" ]]; then
        log "existing sealed-secret key pair kept (idempotent install)"
        return 0
    fi
    log "generating the sealed-secret key pair (private key readable by the worker only)"
    if ! "$bin" keygen --keys-dir "$dir" >/dev/null; then
        fail "key generation failed; run 'tinyship keygen' after fixing the error"
        return 1
    fi
    if ! sandbox; then
        chown root:tinyship "$dir" 2>/dev/null || true
        chown root:tinyship "$dir/secrets.pub" 2>/dev/null || true
        chown tinyship-worker "$dir/secrets.key" 2>/dev/null || true
    fi
    log "installed $dir (secrets.key 0400 worker-only, secrets.pub 0640)"
}

# write_config records the host configuration the services run with.
# The services read these values from their own defaults and from the
# database (budget), so the file documents the effective setup for the
# operator; an existing file is never overwritten.
write_config() {
    local path="$INSTALL_ROOT/etc/tinyship/config.yaml"
    if [[ -e "$path" ]]; then
        log "existing $path kept (idempotent install)"
        return 0
    fi
    cat >"$path" <<'EOF'
# Host configuration, written by the TinyShip installer.
# The services read these values from their own defaults; the host RAM
# budget is applied with `tinyship budget set` and stored in the
# database. This file records the effective configuration.
host:
  budget_mb: auto            # auto = min(50% MemTotal, MemTotal - 1024 MiB)
  disk_guard_free_mb: 1024   # deploys refuse to start below this free space
                             # on the filesystem holding /var/lib/docker
api:
  tcp_listen: 127.0.0.1:7878
  unix_socket: /run/tinyship/api.sock
worker:
  unix_socket: /run/tinyship/worker.sock
  host_port_range: 22500-22999
caddy:
  admin_socket: /run/caddy/admin.sock
paths:
  database: /var/lib/tinyship/tinyship.db
  keys: /etc/tinyship/keys
  token: /etc/tinyship/token
EOF
    chmod 0640 "$path"
    if ! sandbox; then
        chown root:tinyship "$path" 2>/dev/null || true
    fi
    log "installed $path (host configuration)"
}

# write_tmpfiles registers the shared run directory with
# systemd-tmpfiles. A service's mount namespace is configured before
# any of its commands run, so the directory named in ReadWritePaths has
# to exist by then; tmpfiles creates it at boot (and once, here, right
# after install). The directory belongs to neither unit, so stopping
# one service never deletes the other's socket, and its shared group
# lets both services create their sockets in it.
write_tmpfiles() {
    local path="$INSTALL_ROOT/etc/tmpfiles.d/tinyship.conf"
    if [[ -e "$path" ]]; then
        log "existing $path kept (idempotent install)"
        return 0
    fi
    install -d -m 0755 "$(dirname "$path")"
    printf 'd /run/tinyship 0771 tinyship tinyship -\n' >"$path"
    chmod 0644 "$path"
    log "installed $path (creates /run/tinyship at boot, mode 0771 shared group)"
    if ! sandbox; then
        systemd-tmpfiles --create "$path" \
            || { fail "could not create /run/tinyship; run: systemd-tmpfiles --create $path"; return 1; }
    fi
}

# write_units installs the two systemd services. The hardening set
# (private tmp, read-only system files, no new privileges, bounded
# address families and syscalls, memory guardrail, restart on failure)
# follows the platform's documented minimum. The group wiring gives the
# API the admin group so its socket is reachable by the CLI, and gives
# the worker the docker, database and proxy-admin groups. WantedBy
# makes both come back after a reboot. An existing unit is never
# overwritten.
write_units() {
    local api_unit="$INSTALL_ROOT/etc/systemd/system/tinyship-api.service"
    local worker_unit="$INSTALL_ROOT/etc/systemd/system/tinyship-worker.service"

    if [[ -e "$api_unit" || -e "$worker_unit" ]]; then
        log "existing systemd units kept (idempotent install)"
    else
        install -d -m 0755 "$(dirname "$api_unit")"
        cat >"$api_unit" <<'EOF'
[Unit]
Description=TinyShip API server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=tinyship
Group=tinyship-admin
SupplementaryGroups=tinyship tinyship-db
Environment=TINYSHIP_DB=/var/lib/tinyship/tinyship.db
Environment=TINYSHIP_RUN_DIR=/run/tinyship
Environment=TINYSHIP_SECRETS_PUB=/etc/tinyship/keys/secrets.pub
ExecStart=/usr/local/bin/tinyship api
Restart=on-failure
RestartSec=5s
UMask=0007

# Hardening: the API needs the database, the run directory, and
# nothing else on the filesystem.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/tinyship /run/tinyship
CapabilityBoundingSet=
RestrictAddressFamilies=AF_UNIX AF_INET
SystemCallFilter=@system-service
MemoryDenyWriteExecute=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
OOMScoreAdjust=-500
MemoryMax=200M

[Install]
WantedBy=multi-user.target
EOF
        cat >"$worker_unit" <<'EOF'
[Unit]
Description=TinyShip job worker
After=network-online.target docker.service caddy.service
Wants=network-online.target docker.service

[Service]
Type=simple
User=tinyship-worker
Group=tinyship
SupplementaryGroups=docker tinyship-db caddy-admin
Environment=TINYSHIP_DB=/var/lib/tinyship/tinyship.db
Environment=TINYSHIP_RUN_DIR=/run/tinyship
Environment=TINYSHIP_SECRETS_KEY=/etc/tinyship/keys/secrets.key
ExecStart=/usr/local/bin/tinyship worker
Restart=on-failure
RestartSec=5s
UMask=0007

# Hardening: the worker is the most privileged component (it holds the
# docker group), so it gets the same filesystem and syscall limits; the
# Docker and proxy sockets stay reachable through group membership.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/tinyship /run/tinyship
CapabilityBoundingSet=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallFilter=@system-service
MemoryDenyWriteExecute=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
OOMScoreAdjust=-500
MemoryMax=200M

[Install]
WantedBy=multi-user.target
EOF
        chmod 0644 "$api_unit" "$worker_unit"
        log "installed $api_unit and $worker_unit"
    fi

    if ! sandbox; then
        systemctl daemon-reload \
            || { fail "systemd could not load the new units; run: systemctl daemon-reload"; return 1; }
    fi
}

# start_services enables both units, starts them, and waits briefly for
# them to report active; a failure prints the journal tail so the
# operator sees why immediately. Sandbox installs start nothing.
start_services() {
    if sandbox; then
        log "sandbox mode: services not enabled or started"
        return 0
    fi
    log "enabling and starting tinyship-api and tinyship-worker"
    systemctl enable --now tinyship-api tinyship-worker \
        || { fail "services could not be started; see: journalctl -u tinyship-api -u tinyship-worker"; return 1; }
    local i active=""
    for i in $(seq 1 15); do
        if systemctl is-active --quiet tinyship-api && systemctl is-active --quiet tinyship-worker; then
            active=1
            break
        fi
        sleep 1
    done
    if [[ -z "$active" ]]; then
        journalctl -u tinyship-api -u tinyship-worker -n 20 --no-pager >&2 || true
        fail "services did not become active; see the log above"
        return 1
    fi
    log "both services active"
    # The proxy is managed by the operator: reload it only if it is
    # already running so the drop-in takes effect without forcing a
    # start the operator did not ask for.
    if [[ -n "$(resolve_caddy)" ]] && systemctl is-active --quiet caddy; then
        systemctl try-restart caddy \
            || warn "caddy is running but could not be restarted; run: systemctl restart caddy"
    fi
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
        if ! sandbox; then
            # Re-assert the handoff the first run performed: the token
            # file existing means the DB was created by root, and a DB
            # left group-readable only would block the worker's writes
            # (SQLite reports them as a read-only database).
            chown tinyship:tinyship-db "$db" "$db-wal" "$db-shm" 2>/dev/null || true
            chmod 0660 "$db" "$db-wal" "$db-shm" 2>/dev/null || true
        fi
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
        # The token command runs as root, so re-own and reopen the files
        # it created: the DB must be writable by the services' group
        # (0644 would let the group only read it), the token readable by
        # the admin group.
        chown tinyship:tinyship-db "$db" "$db-wal" "$db-shm" 2>/dev/null || true
        chmod 0660 "$db" "$db-wal" "$db-shm" 2>/dev/null || true
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

if ! command -v docker >/dev/null 2>&1; then
    ensure_docker || exit 1
fi
if ! command -v docker >/dev/null 2>&1; then
    fail "Docker is not available; install Docker >= 24.0 and re-run the installer"
    exit 1
fi

install_from_local "$FROM_LOCAL"
