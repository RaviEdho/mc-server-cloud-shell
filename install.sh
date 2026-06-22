#!/usr/bin/env bash
set -Eeuo pipefail

INSTALL_DIR="$HOME/minecraft-server"
MC_VERSION="latest"
LOADER_VERSION="latest"
JAVA_MAJOR=""
AGREE_EULA=0
REUSE=0
FORCE=0
SKIP_PLAYIT_CLAIM=0
START_MONITOR=1
UPDATE_MONITOR=0
PLATFORM="auto"
SERVICE_MODE="auto"
ACTIVE_PLATFORM=""
ACTIVE_SERVICE_MODE=""
DETECTED_OS_ID=""
DETECTED_OS_LIKE=""
SYSTEMD_SERVICE_NAME="minecraft-monitor.service"
MONITOR_PROGRAM_NAME="mc-monitor"

SCRIPT_SOURCE="${BASH_SOURCE[0]:-}"
if [[ -n "$SCRIPT_SOURCE" && -f "$SCRIPT_SOURCE" ]]; then
  SCRIPT_DIR="$(cd -- "$(dirname -- "$SCRIPT_SOURCE")" && pwd)"
else
  SCRIPT_DIR="$(pwd)"
fi
SETUP_DIR=""
PLAYIT_SETUP_PID=""
SETUP_COLOR="1;36"
RAW_REPO_BASE="${RAW_REPO_BASE:-https://raw.githubusercontent.com/RaviEdho/mc-server-cloud-shell/master}"
TEMP_FILES=()
DOWNLOADED_TEMP_FILE=""
MONITOR_SOURCE=""

color_enabled() {
  local fd="$1"
  [[ -z "${SETUP_NO_COLOR:-}" && -t $fd ]]
}

# Logging/output

print_plain() {
  local fd="$1"
  local text="$2"
  if [[ "$fd" -eq 2 ]]; then
    printf '%s' "$text" >&2
  else
    printf '%s' "$text"
  fi
}

print_setup_text() {
  local fd="$1"
  local text="$2"
  local ending="${3:-$'\n'}"

  if [[ -z "$text" ]]; then
    print_plain "$fd" "$ending"
    return 0
  fi

  if color_enabled "$fd"; then
    if [[ "$fd" -eq 2 ]]; then
      printf '\033[%sm%s\033[0m%s' "$SETUP_COLOR" "$text" "$ending" >&2
    else
      printf '\033[%sm%s\033[0m%s' "$SETUP_COLOR" "$text" "$ending"
    fi
  else
    print_plain "$fd" "${text}${ending}"
  fi
}

print_setup_block() {
  local fd="$1"
  local line
  while IFS= read -r line; do
    print_setup_text "$fd" "$line"
  done
}

require_tty() {
  [[ -r /dev/tty && -w /dev/tty ]] || die "$1"
}

log() {
  print_setup_text 1 "[setup] $*"
}

die() {
  print_setup_text 2 "[setup] ERROR: $*"
  exit 1
}

usage() {
  print_setup_block 1 <<'USAGE'
Usage:
  ./install.sh [options]

Options:
  --mc-version VERSION       Minecraft version to install. Default: latest release.
  --java-major VERSION      Override required Java major version.
  --install-dir DIR         Install directory. Default: ~/minecraft-server.
  --agree-eula              Write eula=true without prompting.
  --reuse                   Reuse an existing non-empty install directory.
  --force                   Move an existing install directory aside as a timestamped backup.
  --skip-playit-claim       Skip interactive playit CLI claim flow.
  --no-start                Do not start the monitor at the end.
  --update-monitor          Update/rebuild and restart only the web monitor.
  --update-autostart        Backward-compatible alias for --update-monitor.
  --platform PLATFORM       Platform override. Values: auto, cloudshell, generic-linux.
  --service MODE            Service mode. Values: auto, bashrc, systemd-user, none.
  -h, --help                Show this help.

Examples:
  ./install.sh --agree-eula
  ./install.sh --mc-version 1.21.6 --agree-eula
  ./install.sh --update-monitor
USAGE
}

# Argument parsing

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mc-version)
      MC_VERSION="${2:-}"
      shift 2
      ;;
    --java-major)
      JAVA_MAJOR="${2:-}"
      shift 2
      ;;
    --install-dir)
      INSTALL_DIR="${2:-}"
      shift 2
      ;;
    --agree-eula)
      AGREE_EULA=1
      shift
      ;;
    --reuse)
      REUSE=1
      shift
      ;;
    --force)
      FORCE=1
      shift
      ;;
    --skip-playit-claim)
      SKIP_PLAYIT_CLAIM=1
      shift
      ;;
    --no-start)
      START_MONITOR=0
      shift
      ;;
    --update-monitor|--update-autostart)
      UPDATE_MONITOR=1
      shift
      ;;
    --platform)
      PLATFORM="${2:-}"
      shift 2
      ;;
    --service)
      SERVICE_MODE="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "Unknown option: $1"
      ;;
  esac
done

INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"
INSTALL_DIR="$(mkdir -p "$(dirname "$INSTALL_DIR")" && cd "$(dirname "$INSTALL_DIR")" && pwd)/$(basename "$INSTALL_DIR")"
SETUP_DIR="$INSTALL_DIR/.setup"

cleanup() {
  if [[ -n "${PLAYIT_SETUP_PID:-}" ]] && kill -0 "$PLAYIT_SETUP_PID" 2>/dev/null; then
    kill "$PLAYIT_SETUP_PID" 2>/dev/null || true
  fi
  if [[ "${#TEMP_FILES[@]}" -gt 0 ]]; then
    rm -f "${TEMP_FILES[@]}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Platform/service selection

systemd_user_available() {
  command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1
}

read_os_release() {
  DETECTED_OS_ID=""
  DETECTED_OS_LIKE=""

  [[ -r /etc/os-release ]] || return 0

  local key
  local value
  while IFS='=' read -r key value; do
    value="${value%\"}"
    value="${value#\"}"
    case "$key" in
      ID)
        DETECTED_OS_ID="$value"
        ;;
      ID_LIKE)
        DETECTED_OS_LIKE="$value"
        ;;
    esac
  done < /etc/os-release
}

is_cloud_shell() {
  [[ -n "${CLOUD_SHELL:-}" || -n "${DEVSHELL_PROJECT_ID:-}" || -d "/google/devshell" ]]
}

is_debian_like_linux() {
  [[ "$(uname -s 2>/dev/null || true)" == "Linux" ]] || return 1
  read_os_release

  case "$DETECTED_OS_ID" in
    debian|ubuntu)
      return 0
      ;;
  esac

  [[ " $DETECTED_OS_LIKE " == *" debian "* ]]
}

detect_platform() {
  if is_cloud_shell; then
    printf 'cloudshell\n'
  elif is_debian_like_linux; then
    printf 'generic-linux\n'
  else
    printf 'unknown-linux\n'
  fi
}

platform_label() {
  case "$1" in
    cloudshell)
      printf 'Cloud Shell\n'
      ;;
    generic-linux)
      if [[ -n "$DETECTED_OS_ID" ]]; then
        printf 'generic Linux (%s)\n' "$DETECTED_OS_ID"
      else
        printf 'generic Linux\n'
      fi
      ;;
    unknown-linux)
      printf 'unknown Linux\n'
      ;;
    *)
      printf '%s\n' "$1"
      ;;
  esac
}

resolve_service_mode() {
  case "$SERVICE_MODE" in
    auto)
      case "$ACTIVE_PLATFORM" in
        cloudshell)
          ACTIVE_SERVICE_MODE="bashrc"
          ;;
        generic-linux)
          if systemd_user_available; then
            ACTIVE_SERVICE_MODE="systemd-user"
          else
            ACTIVE_SERVICE_MODE="none"
          fi
          ;;
        *)
          ACTIVE_SERVICE_MODE="none"
          ;;
      esac
      ;;
    bashrc|systemd-user|none)
      ACTIVE_SERVICE_MODE="$SERVICE_MODE"
      ;;
    "" )
      die "--service requires a value."
      ;;
    *)
      die "Unsupported service mode: $SERVICE_MODE"
      ;;
  esac
}

validate_platform_service_mode() {
  case "$ACTIVE_PLATFORM:$ACTIVE_SERVICE_MODE" in
    cloudshell:bashrc|generic-linux:none)
      ;;
    generic-linux:systemd-user)
      systemd_user_available || die "systemd --user is not available. Re-run with --service none to use manual scripts."
      ;;
    cloudshell:systemd-user|cloudshell:none)
      die "Service mode '$ACTIVE_SERVICE_MODE' is not supported for Cloud Shell in this phase."
      ;;
    generic-linux:bashrc)
      die "Service mode 'bashrc' is Cloud Shell-specific; use systemd-user or none for generic Linux."
      ;;
  esac
}

require_supported_install_platform() {
  case "$ACTIVE_PLATFORM" in
    cloudshell)
      return 0
      ;;
    generic-linux)
      return 0
      ;;
    unknown-linux)
      die "Unsupported Linux distribution. Phase 4 starts with Ubuntu/Debian only."
      ;;
    *)
      die "Unsupported platform: $ACTIVE_PLATFORM"
      ;;
  esac
}

resolve_platform() {
  case "$PLATFORM" in
    auto)
      ACTIVE_PLATFORM="$(detect_platform)"
      ;;
    cloudshell)
      ACTIVE_PLATFORM="cloudshell"
      ;;
    generic-linux)
      ACTIVE_PLATFORM="generic-linux"
      ;;
    "" )
      die "--platform requires a value."
      ;;
    *)
      die "Unsupported platform: $PLATFORM"
      ;;
  esac

  resolve_service_mode
  validate_platform_service_mode

  log "Detected platform: $(platform_label "$ACTIVE_PLATFORM")"
  log "Service mode: $ACTIVE_SERVICE_MODE"
}

platform_install_autostart() {
  case "$ACTIVE_PLATFORM:$ACTIVE_SERVICE_MODE" in
    cloudshell:bashrc)
      install_bashrc_hook
      ;;
    generic-linux:systemd-user|generic-linux:none)
      install_generic_linux_service
      ;;
    *)
      die "Unsupported service mode '$ACTIVE_SERVICE_MODE' for platform '$ACTIVE_PLATFORM'."
      ;;
  esac
}

platform_dashboard_hint() {
  case "$ACTIVE_PLATFORM" in
    cloudshell)
      print_setup_block 1 <<'EOF'
Dashboard:
  Open Cloud Shell Web Preview on port 8080.
EOF
      ;;
    generic-linux)
      print_setup_block 1 <<'EOF'
Dashboard:
  The dashboard will bind to 127.0.0.1:8080 by default.
  Use SSH local port forwarding:
    ssh -L 8080:127.0.0.1:8080 user@server
EOF
      ;;
  esac
}

# Common helpers

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
}

run_privileged() {
  if [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
    "$@"
  else
    require_command sudo
    sudo "$@"
  fi
}

download() {
  local url="$1"
  local output="$2"
  log "Downloading $url"
  curl -fL --retry 3 --retry-delay 2 --connect-timeout 20 -o "$output" "$url"
}

download_temp_file() {
  local url="$1"
  local tmp
  tmp="$(mktemp)"
  TEMP_FILES+=("$tmp")
  download "$url" "$tmp" >&2
  DOWNLOADED_TEMP_FILE="$tmp"
}

check_prerequisites() {
  require_command bash
  require_command curl
  require_command python3
  require_command awk
  require_command grep
  require_command sed
  require_command tar
  require_command chmod
  require_command cp
  require_command date
  require_command df
  require_command find
  require_command mktemp
  require_command mv
  require_command ps

  local free_mb
  free_mb="$(df -Pm "$HOME" | awk 'NR==2 {print $4}')"
  if [[ -n "$free_mb" && "$free_mb" -lt 1500 ]]; then
    die "Not enough free disk in HOME filesystem (${free_mb} MB). Free at least 1500 MB first."
  fi
}

# Install directory

prepare_install_dir() {
  if [[ "$FORCE" -eq 1 && "$REUSE" -eq 1 ]]; then
    die "--force and --reuse cannot be used together."
  fi

  if [[ -d "$INSTALL_DIR" && -n "$(find "$INSTALL_DIR" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]]; then
    if [[ "$FORCE" -eq 1 ]]; then
      local backup="${INSTALL_DIR}.backup-$(date +%Y%m%d-%H%M%S)"
      log "Moving existing install directory to $backup"
      mv "$INSTALL_DIR" "$backup"
    elif [[ "$REUSE" -ne 1 ]]; then
      die "$INSTALL_DIR already exists and is not empty. Use --reuse or --force."
    fi
  fi

  mkdir -p "$INSTALL_DIR" "$SETUP_DIR" "$HOME/.config/playit_gg"
}

# Java installation

install_sdkman() {
  export SDKMAN_DIR="$HOME/.sdkman"
  if [[ ! -s "$SDKMAN_DIR/bin/sdkman-init.sh" ]]; then
    log "Installing SDKMAN"
    curl -s "https://get.sdkman.io" | bash
  fi
  # shellcheck disable=SC1091
  set +u
  source "$SDKMAN_DIR/bin/sdkman-init.sh"
  set -u

  if [[ -f "$SDKMAN_DIR/etc/config" ]]; then
    sed -i 's/^sdkman_auto_answer=.*/sdkman_auto_answer=true/' "$SDKMAN_DIR/etc/config" || true
  fi
}

resolve_minecraft_metadata() {
  local manifest="$SETUP_DIR/version_manifest_v2.json"
  local version_json="$SETUP_DIR/version.json"
  download "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json" "$manifest"

  local resolved
  if ! resolved="$(python3 - "$manifest" "$MC_VERSION" 2>&1 <<'PY'
import json, sys
manifest_path, requested = sys.argv[1], sys.argv[2]
with open(manifest_path, "r", encoding="utf-8") as f:
    manifest = json.load(f)
if requested == "latest":
    requested = manifest["latest"]["release"]
for item in manifest["versions"]:
    if item["id"] == requested:
        print(item["id"])
        print(item["url"])
        raise SystemExit(0)
raise SystemExit(f"Minecraft version not found in Mojang manifest: {requested}")
PY
  )"; then
    die "$resolved"
  fi

  RESOLVED_MC_VERSION="$(printf '%s\n' "$resolved" | sed -n '1p')"
  VERSION_URL="$(printf '%s\n' "$resolved" | sed -n '2p')"
  download "$VERSION_URL" "$version_json"

  if [[ -z "$JAVA_MAJOR" ]]; then
    JAVA_MAJOR="$(python3 - "$version_json" <<'PY' || true
import json, sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    data = json.load(f)
value = data.get("javaVersion", {}).get("majorVersion")
if value:
    print(value)
PY
    )"
  fi

  if [[ -z "$JAVA_MAJOR" ]]; then
    JAVA_MAJOR="$(fallback_java_major "$RESOLVED_MC_VERSION")"
    log "Mojang metadata did not provide Java major version; using fallback Java $JAVA_MAJOR"
  fi

  log "Minecraft version: $RESOLVED_MC_VERSION"
  log "Required Java major: $JAVA_MAJOR"
}

fallback_java_major() {
  local version="$1"
  if [[ "$version" =~ ^1\.([0-9]+)(\.([0-9]+))? ]]; then
    local minor="${BASH_REMATCH[1]}"
    local patch="${BASH_REMATCH[3]:-0}"
    if (( minor <= 16 )); then
      printf '8\n'
    elif (( minor == 17 )); then
      printf '16\n'
    elif (( minor < 20 )); then
      printf '17\n'
    elif (( minor == 20 && patch <= 4 )); then
      printf '17\n'
    elif (( minor <= 21 )); then
      printf '21\n'
    else
      printf '25\n'
    fi
  else
    printf '25\n'
  fi
}

select_sdkman_java_identifier() {
  local major="$1"
  local list_file="$SETUP_DIR/sdk-java-list.txt"
  set +u
  sdk list java > "$list_file"
  local sdk_list_status=$?
  set -u
  [[ "$sdk_list_status" -eq 0 ]] || die "Unable to list Java candidates with SDKMAN."

  local id
  id="$(awk -F'|' -v major="$major" '
    NF >= 6 && tolower($0) ~ /tem/ {
      ident=$NF
      gsub(/^[ \t*]+|[ \t]+$/, "", ident)
      if (ident ~ "^" major "(\\.|-).*tem$") {
        print ident
        exit
      }
    }
  ' "$list_file")"

  if [[ -z "$id" ]]; then
    id="$(awk -F'|' -v major="$major" '
      NF >= 6 {
        ident=$NF
        gsub(/^[ \t*]+|[ \t]+$/, "", ident)
        if (ident ~ "^" major "(\\.|-).*$") {
          print ident
          exit
        }
      }
    ' "$list_file")"
  fi

  [[ -n "$id" ]] || die "SDKMAN did not list a Java $major candidate. Check $list_file."
  printf '%s\n' "$id"
}

verify_java_install() {
  local java_output
  java_output="$(java -version 2>&1)" || die "java -version failed after installing Java: $java_output"
  printf '%s\n' "$java_output" | tee "$SETUP_DIR/java-version.txt"
  printf '%s\n' "$java_output" | grep -Eq "version \"(${JAVA_MAJOR}\\.|1\\.${JAVA_MAJOR}\\.)" || {
    log "Warning: could not prove Java major $JAVA_MAJOR from java -version output."
  }
}

install_java_with_apt() {
  [[ "$ACTIVE_PLATFORM" == "generic-linux" ]] || return 1
  command -v apt-get >/dev/null 2>&1 || return 1
  command -v apt-cache >/dev/null 2>&1 || return 1

  local package="openjdk-${JAVA_MAJOR}-jdk-headless"
  if ! apt-cache show "$package" >/dev/null 2>&1; then
    log "APT package $package is not available; falling back to SDKMAN."
    return 1
  fi

  log "Installing Java with APT package: $package"
  run_privileged apt-get update
  run_privileged apt-get install -y "$package"
  hash -r
  verify_java_install
}

install_java_with_sdkman() {
  install_sdkman
  JAVA_IDENTIFIER="$(select_sdkman_java_identifier "$JAVA_MAJOR")"
  log "Installing/selecting Java candidate: $JAVA_IDENTIFIER"
  set +u
  sdk install java "$JAVA_IDENTIFIER" || true
  sdk default java "$JAVA_IDENTIFIER"
  sdk use java "$JAVA_IDENTIFIER" >/dev/null
  set -u
  hash -r

  verify_java_install
}

install_java() {
  if install_java_with_apt; then
    return 0
  fi

  install_java_with_sdkman
}

# Minecraft/Fabric installation

download_fabric_installer() {
  local metadata="$SETUP_DIR/fabric-maven-metadata.xml"
  local version=""
  if curl -fsSL --connect-timeout 20 "https://maven.fabricmc.net/net/fabricmc/fabric-installer/maven-metadata.xml" -o "$metadata"; then
    version="$(python3 - "$metadata" <<'PY' || true
import sys, xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
latest = root.findtext("versioning/release") or root.findtext("versioning/latest")
if latest:
    print(latest)
PY
    )"
  fi
  if [[ -z "$version" ]]; then
    version="1.1.1"
    log "Could not resolve latest Fabric installer; falling back to $version"
  fi

  FABRIC_INSTALLER="$INSTALL_DIR/fabric-installer-$version.jar"
  download "https://maven.fabricmc.net/net/fabricmc/fabric-installer/$version/fabric-installer-$version.jar" "$FABRIC_INSTALLER"
}

install_fabric_server() {
  if [[ -f "$INSTALL_DIR/fabric-server-launch.jar" && "$REUSE" -eq 1 ]]; then
    log "Fabric server launcher already exists; reusing existing server files."
  else
    log "Installing Fabric server into $INSTALL_DIR"
    java -jar "$FABRIC_INSTALLER" server \
      -dir "$INSTALL_DIR" \
      -mcversion "$RESOLVED_MC_VERSION" \
      -loader "$LOADER_VERSION" \
      -downloadMinecraft
  fi

  [[ -f "$INSTALL_DIR/fabric-server-launch.jar" ]] || die "Fabric installer did not create fabric-server-launch.jar."
  if [[ ! -f "$INSTALL_DIR/server.properties" ]]; then
    log "Creating initial server.properties"
    cat > "$INSTALL_DIR/server.properties" <<'PROPS'
server-port=25565
max-players=20
enable-status=true
enable-query=false
enable-rcon=false
rcon.port=25575
rcon.password=
query.port=25565
online-mode=true
motd=A Minecraft Server
PROPS
  fi

  if [[ "$AGREE_EULA" -ne 1 ]]; then
    require_tty "EULA agreement requires an interactive terminal. Rerun with --agree-eula if you agree to https://aka.ms/MinecraftEULA."
    print_setup_text 1 'Do you agree to the Minecraft EULA (https://aka.ms/MinecraftEULA)? Type "yes" to continue: ' ''
    read -r reply < /dev/tty
    [[ "$reply" == "yes" ]] || die "EULA was not accepted."
  fi

  cat > "$INSTALL_DIR/eula.txt" <<'EULA'
# By changing this to true you indicate your agreement to Minecraft's EULA.
# https://aka.ms/MinecraftEULA
eula=true
EULA
}

# playit installation and claim flow

download_playit() {
  download "https://github.com/playit-cloud/playit-agent/releases/latest/download/playit-linux-amd64" "$INSTALL_DIR/playit-linux-amd64"
  download "https://github.com/playit-cloud/playit-agent/releases/latest/download/playit-cli-linux-amd64" "$INSTALL_DIR/playit-cli-linux-amd64"
  chmod +x "$INSTALL_DIR/playit-linux-amd64" "$INSTALL_DIR/playit-cli-linux-amd64"
}

start_playit_setup_daemon() {
  local socket="$1"
  local secret="$2"
  local log_file="$SETUP_DIR/playit-daemon.log"
  rm -f "$socket"
  : > "$log_file"
  log "Starting playit daemon for setup"
  "$INSTALL_DIR/playit-linux-amd64" \
    --socket-path "$socket" \
    --secret-path "$secret" \
    > "$log_file" 2>&1 &
  PLAYIT_SETUP_PID="$!"
  echo "$PLAYIT_SETUP_PID" > "$SETUP_DIR/playit-daemon.pid"

  for _ in $(seq 1 30); do
    [[ -S "$socket" ]] && return 0
    if ! kill -0 "$PLAYIT_SETUP_PID" 2>/dev/null; then
      tail -n 80 "$log_file" >&2 || true
      die "playit daemon exited during setup."
    fi
    sleep 1
  done
  tail -n 80 "$log_file" >&2 || true
  die "playit daemon did not create socket $socket."
}

verify_playit_claim() {
  local secret="$1"
  local log_file="$SETUP_DIR/playit-daemon.log"
  [[ -s "$secret" ]] || die "playit secret was not created at $secret."
  kill -0 "$PLAYIT_SETUP_PID" 2>/dev/null || die "playit daemon is not running after CLI setup."

  for _ in $(seq 1 45); do
    if grep -Eq "playit connected|tunnels loaded|account_status" "$log_file"; then
      log "playit setup verified."
      return 0
    fi
    sleep 1
  done

  tail -n 120 "$log_file" >&2 || true
  die "playit daemon did not report a connected/verified account."
}

setup_playit_claim() {
  local socket="/tmp/playit.sock"
  local secret="$HOME/.config/playit_gg/playit.toml"

  if [[ "$SKIP_PLAYIT_CLAIM" -eq 1 ]]; then
    log "Skipping interactive playit claim flow."
    return 0
  fi

  start_playit_setup_daemon "$socket" "$secret"

  if [[ ! -s "$secret" ]]; then
    require_tty "playit first-time setup requires an interactive terminal. Rerun with --skip-playit-claim to skip it."
    log "Running playit setup. Open the claim link it prints; setup will continue after the claim completes."
    set +e
    "$INSTALL_DIR/playit-cli-linux-amd64" --socket-path "$socket" setup < /dev/tty
    local cli_code=$?
    set -e
    if [[ "$cli_code" -ne 0 ]]; then
      log "playit CLI exited with code $cli_code; verifying daemon/secret state anyway."
    fi
  else
    log "Existing playit secret found; verifying daemon connection."
  fi

  verify_playit_claim "$secret"
  kill "$PLAYIT_SETUP_PID" 2>/dev/null || true
  wait "$PLAYIT_SETUP_PID" 2>/dev/null || true
  PLAYIT_SETUP_PID=""
  rm -f "$socket"
}

# Monitor installation/update

install_monitor() {
  update_monitor_program
  MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/$MONITOR_PROGRAM_NAME" -configure
}

resolve_monitor_source() {
  local local_source="$SCRIPT_DIR/monitor/monitor.py"
  if [[ -f "$local_source" ]]; then
    MONITOR_SOURCE="$local_source"
    return 0
  fi

  download_temp_file "$RAW_REPO_BASE/monitor/monitor.py"
  MONITOR_SOURCE="$DOWNLOADED_TEMP_FILE"
}

update_monitor_program() {
  local python_source
  local installed_binary="$INSTALL_DIR/$MONITOR_PROGRAM_NAME"
  local source_updated=0

  resolve_monitor_source || die "Unable to resolve monitor source."
  python_source="$MONITOR_SOURCE"
  [[ -f "$python_source" ]] || die "Missing monitor source: $python_source"

  mkdir -p "$INSTALL_DIR"

  if [[ ! -f "$installed_binary" || "$python_source" -nt "$installed_binary" ]]; then
    log "Updating Python monitor: $installed_binary"
    cp "$python_source" "$installed_binary"
    chmod +x "$installed_binary"
    source_updated=1
  else
    log "Python monitor is already current."
  fi

  if [[ "$source_updated" -eq 0 ]]; then
    log "No monitor update needed."
  fi

  cleanup_legacy_monitor_files
}

cleanup_legacy_monitor_files() {
  local legacy_source="$INSTALL_DIR/cloudshell-mc-autostart.go"
  local legacy_binary="$INSTALL_DIR/cloudshell-mc-autostart"
  local go_source="$INSTALL_DIR/cloudshell-mc-monitor.go"
  local go_binary="$INSTALL_DIR/cloudshell-mc-monitor"
  local go_cache="$INSTALL_DIR/.gocache"

  if [[ -e "$legacy_source" || -e "$legacy_binary" || -e "$go_source" || -e "$go_binary" || -e "$go_cache" ]]; then
    log "Removing legacy Go monitor files."
    rm -rf "$legacy_source" "$legacy_binary" "$go_source" "$go_binary" "$go_cache"
  fi
}

# Cloud Shell-compatible autostart

install_bashrc_hook() {
  local bashrc="$HOME/.bashrc"
  local tmp
  local tmp2
  tmp="$(mktemp)"
  tmp2="$(mktemp)"
  touch "$bashrc"

  awk '
    /# >>> minecraft-cloudshell-monitor >>>/ {skip=1; next}
    /# <<< minecraft-cloudshell-monitor <<</ {skip=0; next}
    /# >>> minecraft-cloudshell-autostart >>>/ {skip=1; next}
    /# <<< minecraft-cloudshell-autostart <<</ {skip=0; next}
    /# >>> minecraft-monitor >>>/ {skip=1; next}
    /# <<< minecraft-monitor <<</ {skip=0; next}
    !skip {print}
  ' "$bashrc" > "$tmp"

  local block
  block="$(cat <<EOF
# >>> minecraft-monitor >>>
if [ -x "$INSTALL_DIR/$MONITOR_PROGRAM_NAME" ]; then
  MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/$MONITOR_PROGRAM_NAME" -start >/dev/null 2>&1
fi
# <<< minecraft-monitor <<<
EOF
)"

  if grep -q '#THIS MUST BE AT THE END OF THE FILE FOR SDKMAN TO WORK!!!' "$tmp"; then
    awk -v block="$block" '
      /#THIS MUST BE AT THE END OF THE FILE FOR SDKMAN TO WORK!!!/ && !inserted {
        print block
        print ""
        inserted=1
      }
      {print}
    ' "$tmp" > "$tmp2"
    mv "$tmp2" "$bashrc"
  else
    cat "$tmp" > "$bashrc"
    printf '\n%s\n' "$block" >> "$bashrc"
  fi
  rm -f "$tmp" "$tmp2"
}

systemd_quote() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "$value"
}

install_control_scripts() {
  local monitor="$INSTALL_DIR/$MONITOR_PROGRAM_NAME"
  local addr="${1:-127.0.0.1:8080}"

  cat > "$INSTALL_DIR/start.sh" <<EOF
#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="\$(cd -- "\$(dirname -- "\${BASH_SOURCE[0]}")" && pwd)"
export MC_MONITOR_ROOT="\$ROOT"
export MC_MONITOR_ADDR="\${MC_MONITOR_ADDR:-$addr}"
exec "\$ROOT/$MONITOR_PROGRAM_NAME" -start
EOF

  cat > "$INSTALL_DIR/stop.sh" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
export MC_MONITOR_ROOT="$ROOT"
exec "$ROOT/$MONITOR_PROGRAM_NAME" -stop
EOF

  cat > "$INSTALL_DIR/status.sh" <<EOF
#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="\$(cd -- "\$(dirname -- "\${BASH_SOURCE[0]}")" && pwd)"
export MC_MONITOR_ROOT="\$ROOT"
export MC_MONITOR_ADDR="\${MC_MONITOR_ADDR:-$addr}"
exec "\$ROOT/$MONITOR_PROGRAM_NAME" -status
EOF

  chmod +x "$INSTALL_DIR/start.sh" "$INSTALL_DIR/stop.sh" "$INSTALL_DIR/status.sh"
  [[ -x "$monitor" ]] || die "Monitor binary is not executable: $monitor"
}

install_systemd_user_service() {
  systemd_user_available || die "systemd --user is not available. Re-run with --service none to use manual scripts."

  local service_dir="$HOME/.config/systemd/user"
  local service_path="$service_dir/$SYSTEMD_SERVICE_NAME"
  local monitor="$INSTALL_DIR/$MONITOR_PROGRAM_NAME"

  mkdir -p "$service_dir"
  cat > "$service_path" <<EOF
[Unit]
Description=Minecraft server monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$(systemd_quote "$INSTALL_DIR")
Environment=$(systemd_quote "MC_MONITOR_ROOT=$INSTALL_DIR")
Environment=$(systemd_quote "MC_MONITOR_ADDR=127.0.0.1:8080")
ExecStart=$(systemd_quote "$monitor") -daemon
ExecStop=$(systemd_quote "$monitor") -stop
Restart=on-failure
RestartSec=5
TimeoutStopSec=45

[Install]
WantedBy=default.target
EOF

  systemctl --user daemon-reload
  systemctl --user enable "$SYSTEMD_SERVICE_NAME"
  log "Installed systemd user service: $service_path"
}

install_generic_linux_service() {
  install_control_scripts "127.0.0.1:8080"

  if [[ "$ACTIVE_SERVICE_MODE" == "systemd-user" ]]; then
    install_systemd_user_service
  else
    log "Skipping systemd service setup; use $INSTALL_DIR/start.sh to start the monitor."
  fi
}

check_port_available() {
  local port="$1"
  if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | awk '{print $4}' | grep -Eq "(^|:)${port}$"; then
    die "Port $port is already in use."
  fi
}

platform_stop_monitor() {
  case "$ACTIVE_PLATFORM:$ACTIVE_SERVICE_MODE" in
    generic-linux:systemd-user)
      systemctl --user stop "$SYSTEMD_SERVICE_NAME" >/dev/null 2>&1 || true
      ;;
    *)
      MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/$MONITOR_PROGRAM_NAME" -stop >/dev/null 2>&1 || true
      ;;
  esac
}

platform_start_monitor() {
  case "$ACTIVE_PLATFORM:$ACTIVE_SERVICE_MODE" in
    generic-linux:systemd-user)
      systemctl --user start "$SYSTEMD_SERVICE_NAME"
      ;;
    generic-linux:none)
      MC_MONITOR_ROOT="$INSTALL_DIR" MC_MONITOR_ADDR="127.0.0.1:8080" "$INSTALL_DIR/$MONITOR_PROGRAM_NAME" -start
      ;;
    cloudshell:bashrc)
      MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/$MONITOR_PROGRAM_NAME" -start
      ;;
    *)
      die "Unsupported monitor start mode: $ACTIVE_PLATFORM/$ACTIVE_SERVICE_MODE"
      ;;
  esac
}

platform_restart_monitor_only() {
  case "$ACTIVE_PLATFORM:$ACTIVE_SERVICE_MODE" in
    generic-linux:systemd-user)
      systemctl --user restart "$SYSTEMD_SERVICE_NAME"
      ;;
    generic-linux:none)
      MC_MONITOR_ROOT="$INSTALL_DIR" MC_MONITOR_ADDR="127.0.0.1:8080" "$INSTALL_DIR/$MONITOR_PROGRAM_NAME" -restart monitor
      ;;
    cloudshell:bashrc)
      MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/$MONITOR_PROGRAM_NAME" -restart monitor
      ;;
    *)
      die "Unsupported monitor restart mode: $ACTIVE_PLATFORM/$ACTIVE_SERVICE_MODE"
      ;;
  esac
}

# Verification and entry points

start_and_verify_monitor() {
  if [[ "$START_MONITOR" -ne 1 ]]; then
    log "Skipping monitor start because --no-start was provided."
    return 0
  fi

  platform_stop_monitor
  sleep 2
  check_port_available 8080
  check_port_available 25565
  check_port_available 25575
  platform_start_monitor

  local status_url="http://127.0.0.1:8080/api/status"
  for _ in $(seq 1 90); do
    if curl -fsS "$status_url" > "$SETUP_DIR/status.json" 2>/dev/null; then
      if grep -q '"portOpen":true' "$SETUP_DIR/status.json"; then
        log "Monitor and Minecraft are responding."
        return 0
      fi
    fi
    sleep 2
  done

  tail -n 120 "$INSTALL_DIR/.runtime/supervisor.log" >&2 || true
  tail -n 120 "$INSTALL_DIR/.runtime/minecraft.log" >&2 || true
  die "Monitor started, but Minecraft did not become healthy in time."
}

update_monitor_only() {
  require_command bash
  require_command curl
  require_command cp
  require_command chmod
  require_command mktemp
  require_command mkdir
  require_command rm

  [[ -d "$INSTALL_DIR" ]] || die "Install directory does not exist: $INSTALL_DIR"
  update_monitor_program
  platform_install_autostart
  log "Restarting monitor without stopping Minecraft."
  platform_restart_monitor_only
  log "Monitor update complete."
}

main() {
  resolve_platform
  require_supported_install_platform

  if [[ "$UPDATE_MONITOR" -eq 1 ]]; then
    update_monitor_only
    return 0
  fi

  check_prerequisites
  prepare_install_dir
  resolve_minecraft_metadata
  install_java
  download_fabric_installer
  install_fabric_server
  download_playit
  setup_playit_claim
  install_monitor
  platform_install_autostart
  start_and_verify_monitor

  print_setup_block 1 <<EOF

Setup complete.

Install directory:
  $INSTALL_DIR

Useful commands:
  cd "$INSTALL_DIR"
  ./$MONITOR_PROGRAM_NAME -status
  ./$MONITOR_PROGRAM_NAME -stop
  ./$MONITOR_PROGRAM_NAME -start

Logs:
  tail -f "$INSTALL_DIR/.runtime/supervisor.log"
  tail -f "$INSTALL_DIR/.runtime/minecraft.log"
  tail -f "$INSTALL_DIR/.runtime/playit.log"
EOF
  platform_dashboard_hint
}

main "$@"
