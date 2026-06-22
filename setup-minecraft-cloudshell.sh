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

SCRIPT_SOURCE="${BASH_SOURCE[0]:-}"
if [[ -n "$SCRIPT_SOURCE" && -f "$SCRIPT_SOURCE" ]]; then
  SCRIPT_DIR="$(cd -- "$(dirname -- "$SCRIPT_SOURCE")" && pwd)"
else
  SCRIPT_DIR="$(pwd)"
fi
SETUP_DIR=""
PLAYIT_SETUP_PID=""
SETUP_COLOR="1;36"
SETUP_ERROR_COLOR="1;31"
RAW_REPO_BASE="${RAW_REPO_BASE:-https://raw.githubusercontent.com/RaviEdho/mc-server-cloud-shell/master}"
TEMP_FILES=()
DOWNLOADED_TEMP_FILE=""
MONITOR_SOURCE=""

color_enabled() {
  local fd="$1"
  [[ -z "${SETUP_NO_COLOR:-}" && -t $fd ]]
}

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
  local color="${4:-$SETUP_COLOR}"

  if [[ -z "$text" ]]; then
    print_plain "$fd" "$ending"
    return 0
  fi

  if color_enabled "$fd"; then
    if [[ "$fd" -eq 2 ]]; then
      printf '\033[%sm%s\033[0m%s' "$color" "$text" "$ending" >&2
    else
      printf '\033[%sm%s\033[0m%s' "$color" "$text" "$ending"
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
  print_setup_text 2 "[setup] ERROR: $*" $'\n' "$SETUP_ERROR_COLOR"
  exit 1
}

usage() {
  print_setup_block 1 <<'USAGE'
Usage:
  ./setup-minecraft-cloudshell.sh [options]

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
  -h, --help                Show this help.

Examples:
  ./setup-minecraft-cloudshell.sh --agree-eula
  ./setup-minecraft-cloudshell.sh --mc-version 1.21.6 --agree-eula
  ./setup-minecraft-cloudshell.sh --update-monitor
USAGE
}

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

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
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
  require_command go

  local free_mb
  free_mb="$(df -Pm "$HOME" | awk 'NR==2 {print $4}')"
  if [[ -n "$free_mb" && "$free_mb" -lt 1500 ]]; then
    die "Not enough free disk in HOME filesystem (${free_mb} MB). Free at least 1500 MB first."
  fi
}

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

install_java() {
  JAVA_IDENTIFIER="$(select_sdkman_java_identifier "$JAVA_MAJOR")"
  log "Installing/selecting Java candidate: $JAVA_IDENTIFIER"
  set +u
  sdk install java "$JAVA_IDENTIFIER" || true
  sdk default java "$JAVA_IDENTIFIER"
  sdk use java "$JAVA_IDENTIFIER" >/dev/null
  set -u
  hash -r

  local java_output
  java_output="$(java -version 2>&1)" || die "java -version failed after installing $JAVA_IDENTIFIER: $java_output"
  printf '%s\n' "$java_output" | tee "$SETUP_DIR/java-version.txt"
  printf '%s\n' "$java_output" | grep -Eq "version \"(${JAVA_MAJOR}\\.|1\\.${JAVA_MAJOR}\\.)" || {
    log "Warning: could not prove Java major $JAVA_MAJOR from java -version output."
  }
}

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

install_monitor() {
  update_monitor_program
  MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/cloudshell-mc-monitor" -configure
}

resolve_monitor_source() {
  local local_source="$SCRIPT_DIR/cloudshell-mc-monitor.go"
  if [[ -f "$local_source" ]]; then
    MONITOR_SOURCE="$local_source"
    return 0
  fi

  download_temp_file "$RAW_REPO_BASE/cloudshell-mc-monitor.go"
  MONITOR_SOURCE="$DOWNLOADED_TEMP_FILE"
}

update_monitor_program() {
  local go_source
  local installed_source="$INSTALL_DIR/cloudshell-mc-monitor.go"
  local installed_binary="$INSTALL_DIR/cloudshell-mc-monitor"
  local source_updated=0
  local rebuilt=0

  resolve_monitor_source || die "Unable to resolve monitor source."
  go_source="$MONITOR_SOURCE"
  [[ -f "$go_source" ]] || die "Missing monitor source: $go_source"

  mkdir -p "$INSTALL_DIR" "$INSTALL_DIR/.gocache"

  if [[ ! -f "$installed_source" || "$go_source" -nt "$installed_source" ]]; then
    log "Updating monitor source: $installed_source"
    cp "$go_source" "$installed_source"
    source_updated=1
  else
    log "Monitor source is already current."
  fi

  if [[ "$source_updated" -eq 1 || ! -x "$installed_binary" || "$installed_source" -nt "$installed_binary" ]]; then
    log "Building monitor"
    GOCACHE="$INSTALL_DIR/.gocache" go build -o "$installed_binary" "$installed_source"
    chmod +x "$installed_binary"
    rebuilt=1
  else
    log "Monitor binary is already current."
  fi

  if [[ "$source_updated" -eq 0 && "$rebuilt" -eq 0 ]]; then
    log "No monitor update needed."
  fi

  cleanup_legacy_monitor_files
}

cleanup_legacy_monitor_files() {
  local legacy_source="$INSTALL_DIR/cloudshell-mc-autostart.go"
  local legacy_binary="$INSTALL_DIR/cloudshell-mc-autostart"

  if [[ -e "$legacy_source" || -e "$legacy_binary" ]]; then
    log "Removing legacy monitor filenames."
    rm -f "$legacy_source" "$legacy_binary"
  fi
}

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
    !skip {print}
  ' "$bashrc" > "$tmp"

  local block
  block="$(cat <<EOF
# >>> minecraft-cloudshell-monitor >>>
if [ -x "$INSTALL_DIR/cloudshell-mc-monitor" ]; then
  MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/cloudshell-mc-monitor" -start >/dev/null 2>&1
fi
# <<< minecraft-cloudshell-monitor <<<
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

check_port_available() {
  local port="$1"
  if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | awk '{print $4}' | grep -Eq "(^|:)${port}$"; then
    die "Port $port is already in use."
  fi
}

start_and_verify_monitor() {
  if [[ "$START_MONITOR" -ne 1 ]]; then
    log "Skipping monitor start because --no-start was provided."
    return 0
  fi

  MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/cloudshell-mc-monitor" -stop >/dev/null 2>&1 || true
  sleep 2
  check_port_available 8080
  check_port_available 25565
  check_port_available 25575
  MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/cloudshell-mc-monitor" -start

  local status_url="http://127.0.0.1:8080/api/status"
  for _ in $(seq 1 90); do
    if curl -fsS "$status_url" > "$SETUP_DIR/status.json" 2>/dev/null; then
      if grep -Eq '"portOpen"[[:space:]]*:[[:space:]]*true' "$SETUP_DIR/status.json"; then
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
  require_command go
  require_command mktemp
  require_command mkdir
  require_command rm

  [[ -d "$INSTALL_DIR" ]] || die "Install directory does not exist: $INSTALL_DIR"
  update_monitor_program
  install_bashrc_hook
  log "Restarting monitor without stopping Minecraft."
  MC_MONITOR_ROOT="$INSTALL_DIR" "$INSTALL_DIR/cloudshell-mc-monitor" -restart monitor
  log "Monitor update complete."
}

main() {
  if [[ "$UPDATE_MONITOR" -eq 1 ]]; then
    update_monitor_only
    return 0
  fi

  check_prerequisites
  prepare_install_dir
  install_sdkman
  resolve_minecraft_metadata
  install_java
  download_fabric_installer
  install_fabric_server
  download_playit
  setup_playit_claim
  install_monitor
  install_bashrc_hook
  start_and_verify_monitor

  print_setup_block 1 <<EOF

Setup complete.

Install directory:
  $INSTALL_DIR

Dashboard:
  Open Cloud Shell Web Preview on port 8080.

Useful commands:
  cd "$INSTALL_DIR"
  ./cloudshell-mc-monitor -status
  ./cloudshell-mc-monitor -stop
  ./cloudshell-mc-monitor -start

Logs:
  tail -f "$INSTALL_DIR/.runtime/supervisor.log"
  tail -f "$INSTALL_DIR/.runtime/minecraft.log"
  tail -f "$INSTALL_DIR/.runtime/playit.log"
EOF
}

main "$@"
