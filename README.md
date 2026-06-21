# Minecraft Server Installer

This project bootstraps a Fabric Minecraft server, connects it through playit.gg, and installs a monitor that supervises the server and exposes a web dashboard.

The current installer path supports Google Cloud Shell. The rewrite is moving toward a generic Ubuntu/Debian VM installer while keeping Cloud Shell supported.

It is currently organized around these main files:

- `install.sh`: one-time bootstrap script for a fresh install
- `setup-minecraft-cloudshell.sh`: legacy Cloud Shell installer entry point kept during the rewrite
- `cloudshell-mc-monitor.go`: Go supervisor and web monitoring dashboard

## Quick Start

Run the installer directly from GitHub:

```bash
curl -fsSL https://raw.githubusercontent.com/RaviEdho/mc-server-cloud-shell/master/install.sh | bash
```

The setup script downloads the monitor source when needed, reads interactive prompts from the terminal, and cleans up temporary installer files automatically.

If you already cloned the repository, run:

```bash
chmod +x install.sh
./install.sh --agree-eula
```

To install a specific Minecraft version:

```bash
./install.sh --mc-version 1.21.6 --agree-eula
```

After setup completes, open Cloud Shell Web Preview on port `8080`.

## What The Setup Script Does

`install.sh` performs the full bootstrap:

1. Checks required commands and available disk space.
2. Creates the install directory, defaulting to `~/minecraft-server`.
3. Installs SDKMAN if it is missing.
4. Resolves the requested Minecraft version.
5. Resolves the required Java major version.
6. Installs and selects that Java version through SDKMAN.
7. Downloads the Fabric installer.
8. Installs the Fabric server files.
9. Writes `eula=true` only after explicit agreement.
10. Downloads both playit binaries.
11. Runs the interactive playit claim flow.
12. Builds the Go monitor.
13. Enables RCON/query in `server.properties`.
14. Adds an idempotent `.bashrc` autostart block.
15. Starts the monitor and verifies the server becomes reachable.

## Options

```text
--mc-version VERSION       Minecraft version to install. Default: latest release.
--java-major VERSION       Override required Java major version.
--install-dir DIR          Install directory. Default: ~/minecraft-server.
--agree-eula               Write eula=true without prompting.
--reuse                    Reuse an existing non-empty install directory.
--force                    Move an existing install directory aside as a timestamped backup.
--skip-playit-claim        Skip interactive playit CLI claim flow.
--no-start                 Do not start the monitor at the end.
--update-monitor           Update/rebuild and restart only the web monitor.
--update-autostart         Backward-compatible alias for --update-monitor.
--platform PLATFORM        Platform override. Values: auto, cloudshell, generic-linux.
--service MODE             Service mode. Values: auto, bashrc, systemd-user, none.
-h, --help                 Show help.
```

On generic Linux, `auto` uses `systemd-user` when a user systemd session is available and falls back to manual scripts otherwise. The generic Linux path still uses the current Go monitor until the Python monitor replacement phase.

## Generic Linux Service Modes

On Ubuntu/Debian-style Linux, the installer writes manual control scripts into the install directory:

```text
~/minecraft-server/start.sh
~/minecraft-server/stop.sh
~/minecraft-server/status.sh
```

When `systemd --user` is available, `--service auto` installs and enables:

```text
~/.config/systemd/user/minecraft-monitor.service
```

The generic Linux dashboard binds to `127.0.0.1:8080` by default. Use SSH local port forwarding to view it without opening a public dashboard port:

```bash
ssh -L 8080:127.0.0.1:8080 user@server
```

## Java Version Resolution

The script first downloads Mojang version metadata and reads:

```text
javaVersion.majorVersion
```

If Mojang metadata does not provide a Java version, the script falls back to an embedded mapping:

```text
<= 1.16.5              Java 8
1.17.x                 Java 16
1.18.x - 1.20.4        Java 17
1.20.5 - 1.21.x        Java 21
newer/future versions  Java 25
```

You can override this with:

```bash
./install.sh --java-major 25 --agree-eula
```

The script then asks SDKMAN for a matching Java candidate, preferring Temurin builds when available.

## Minecraft And Fabric Install

The script installs the server directly into:

```text
~/minecraft-server
```

Expected files include:

```text
fabric-server-launch.jar
server.jar
server.properties
eula.txt
mods/
world/
```

The Fabric installer is resolved from Fabric Maven metadata. If that lookup fails, the script falls back to Fabric installer `1.1.1`.

On a fresh Fabric install, `server.properties` may not exist until the server is started for the first time. The setup script creates a minimal initial `server.properties` in that case so RCON/query settings can be configured before the monitor starts the server.

## EULA Handling

The script does not silently accept the Minecraft EULA.

Use:

```bash
./install.sh --agree-eula
```

only if you agree to:

```text
https://aka.ms/MinecraftEULA
```

Without `--agree-eula`, the script prompts interactively.

## playit Setup

The script downloads:

```text
playit-linux-amd64
playit-cli-linux-amd64
```

During first-time setup it starts the daemon in the background:

```bash
./playit-linux-amd64 \
  --socket-path /tmp/playit.sock \
  --secret-path ~/.config/playit_gg/playit.toml
```

Then it runs the first-time setup command in the foreground:

```bash
./playit-cli-linux-amd64 --socket-path /tmp/playit.sock setup
```

Open the printed playit claim link. After the claim completes and the CLI exits, the setup script verifies:

- `~/.config/playit_gg/playit.toml` exists and is non-empty
- the playit daemon is still running
- the daemon log reports a connected account or loaded tunnels

If you already have a valid playit secret, the script reuses it.

## Monitor And Autostart

The Go monitor is built as:

```text
~/minecraft-server/cloudshell-mc-monitor
```

It supervises:

- Fabric Minecraft server
- playit daemon
- local web dashboard
- machine/server metrics collection

It also creates:

```text
~/minecraft-server/.runtime/
~/minecraft-server/.monitor/
```

The setup script adds this managed block to `~/.bashrc`:

```bash
# >>> minecraft-cloudshell-monitor >>>
if [ -x "$HOME/minecraft-server/cloudshell-mc-monitor" ]; then
  MC_MONITOR_ROOT="$HOME/minecraft-server" "$HOME/minecraft-server/cloudshell-mc-monitor" -start >/dev/null 2>&1
fi
# <<< minecraft-cloudshell-monitor <<<
```

That means a new Cloud Shell session starts the monitor automatically. Cloud Shell still cannot run while the Cloud Shell VM is stopped due to inactivity.

## Dashboard

Open Cloud Shell Web Preview on port:

```text
8080
```

The dashboard shows:

- CPU usage
- memory usage
- disk usage
- Minecraft process status
- playit process status
- Minecraft version
- current/max players
- TPS/MSPT
- playit endpoint
- recent/searchable logs
- live Minecraft process logs
- RCON command input and command response output

It can also control:

- start Minecraft
- stop Minecraft gracefully through RCON
- restart Minecraft
- kill Minecraft if graceful stop hangs
- start/stop/restart/kill playit

## RCON And Query

The monitor enables these in `server.properties`:

```properties
enable-rcon=true
enable-query=true
rcon.port=25575
query.port=25565
```

The RCON password is generated at:

```text
~/minecraft-server/.runtime/rcon.password
```

The dashboard uses RCON for:

```text
list
tick query
stop
dashboard command input
```

The monitor keeps a persistent RCON connection and polls Minecraft health every 30 seconds, so it should not spam the Minecraft logs with repeated RCON connect/disconnect messages.

## Runtime Commands

From the install directory:

```bash
cd ~/minecraft-server
```

Check status:

```bash
./cloudshell-mc-monitor -status
```

Start monitor:

```bash
./cloudshell-mc-monitor -start
```

Stop monitor:

```bash
./cloudshell-mc-monitor -stop
```

Restart all services:

```bash
./cloudshell-mc-monitor -restart
./cloudshell-mc-monitor -restart all
```

Restart only the web monitor without stopping Minecraft:

```bash
./cloudshell-mc-monitor -restart monitor
./cloudshell-mc-monitor -restart mon
./cloudshell-mc-monitor -restart web
```

Restart only Minecraft:

```bash
./cloudshell-mc-monitor -restart minecraft
./cloudshell-mc-monitor -restart mc
./cloudshell-mc-monitor -restart server
```

Restart only playit:

```bash
./cloudshell-mc-monitor -restart playit
./cloudshell-mc-monitor -restart conn
./cloudshell-mc-monitor -restart connection
```

Apply RCON/query config again:

```bash
./cloudshell-mc-monitor -configure
```

The monitor helper supports these modes:

```text
-start                  Start the monitor daemon if it is not already running.
-daemon                 Internal foreground mode used by start.
-stop                   Stop the monitor, Minecraft server, and playit daemon.
-restart [all]                    Restart the monitor, Minecraft server, and playit daemon.
-restart monitor|mon|web          Restart only the web monitor process and keep Minecraft running.
-restart minecraft|mc|server      Restart only Minecraft through the running monitor.
-restart playit|conn|connection   Restart only the playit daemon through the running monitor.
-status                 Print process and server status.
-configure              Reapply server.properties RCON/query settings.
```

The older `-mode <action>` form still works for compatibility.

## Logs

Monitor logs:

```bash
tail -f ~/minecraft-server/.runtime/supervisor.log
```

Minecraft supervised process logs:

```bash
tail -f ~/minecraft-server/.runtime/minecraft.log
```

playit supervised process logs:

```bash
tail -f ~/minecraft-server/.runtime/playit.log
```

Minecraft server log:

```bash
tail -f ~/minecraft-server/logs/latest.log
```

## Metrics Storage

Machine metrics are sampled every 30 seconds and stored in:

```text
~/minecraft-server/.monitor/metrics.jsonl
```

The monitor keeps roughly 7 days of samples and prunes older entries.

## Rerunning The Setup Script

If `~/minecraft-server` already exists and is non-empty, the script refuses to continue by default.

Reuse the existing directory:

```bash
./install.sh --reuse --agree-eula
```

Move the existing directory aside and create a fresh one:

```bash
./install.sh --force --agree-eula
```

Update only the monitor source/binary, refresh the `.bashrc` hook, and restart the web monitor without stopping Minecraft:

```bash
./install.sh --update-monitor
```

The old flag still works as an alias:

```bash
./install.sh --update-autostart
```

The `.bashrc` monitor block is idempotent. Rerunning the script replaces the old managed block instead of appending duplicates.

## Common Failure Cases

Network failures can break SDKMAN, Mojang metadata, Fabric Maven, or playit downloads. Rerun the script after connectivity returns.

If Java installs incorrectly, verify:

```bash
source "$HOME/.sdkman/bin/sdkman-init.sh"
java -version
```

If playit setup fails, inspect:

```bash
tail -n 120 ~/minecraft-server/.setup/playit-daemon.log
```

If the monitor starts but Minecraft does not become healthy, inspect:

```bash
tail -n 120 ~/minecraft-server/.runtime/supervisor.log
tail -n 120 ~/minecraft-server/.runtime/minecraft.log
```

If a port is already used, stop the conflicting process or change the relevant config:

```text
25565  Minecraft
25575  RCON
8080   dashboard
/tmp/playit.sock  playit IPC socket
```

## Cloud Shell Limitations

This is suitable for testing and lightweight personal use, but Cloud Shell is not a reliable always-on host.

Cloud Shell can stop after inactivity. Files under `$HOME` persist, but running processes stop. When you reopen Cloud Shell, the `.bashrc` hook starts the monitor again.
