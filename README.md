# Minecraft Cloud Shell Server Scaffold

This scaffold bootstraps a Fabric Minecraft server in Google Cloud Shell, connects it through playit.gg, and installs a Go monitor that autostarts the server and exposes a Cloud Shell Web Preview dashboard.

It is designed around two source files:

- `setup-minecraft-cloudshell.sh`: one-time bootstrap script for a fresh Cloud Shell `$HOME`
- `cloudshell-mc-autostart.go`: Go supervisor and web monitoring dashboard

## Quick Start

Put both files in the same directory, then run:

```bash
chmod +x setup-minecraft-cloudshell.sh
./setup-minecraft-cloudshell.sh --agree-eula
```

To install a specific Minecraft version:

```bash
./setup-minecraft-cloudshell.sh --mc-version 1.21.6 --agree-eula
```

After setup completes, open Cloud Shell Web Preview on port `8080`.

## What The Setup Script Does

`setup-minecraft-cloudshell.sh` performs the full bootstrap:

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
-h, --help                 Show help.
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
./setup-minecraft-cloudshell.sh --java-major 25 --agree-eula
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

## EULA Handling

The script does not silently accept the Minecraft EULA.

Use:

```bash
./setup-minecraft-cloudshell.sh --agree-eula
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

Then it runs the CLI in the foreground:

```bash
./playit-cli-linux-amd64 --socket-path /tmp/playit.sock
```

Follow the playit login/claim instructions. After the CLI exits, the setup script verifies:

- `~/.config/playit_gg/playit.toml` exists and is non-empty
- the playit daemon is still running
- the daemon log reports a connected account or loaded tunnels

If you already have a valid playit secret, the script reuses it.

## Monitor And Autostart

The Go monitor is built as:

```text
~/minecraft-server/cloudshell-mc-autostart
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
# >>> minecraft-cloudshell-autostart >>>
if [ -x "$HOME/minecraft-server/cloudshell-mc-autostart" ]; then
  MC_AUTOSTART_ROOT="$HOME/minecraft-server" "$HOME/minecraft-server/cloudshell-mc-autostart" -mode start >/dev/null 2>&1
fi
# <<< minecraft-cloudshell-autostart <<<
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
```

The monitor keeps a persistent RCON connection and polls Minecraft health every 30 seconds, so it should not spam the Minecraft logs with repeated RCON connect/disconnect messages.

## Runtime Commands

From the install directory:

```bash
cd ~/minecraft-server
```

Check status:

```bash
./cloudshell-mc-autostart -mode status
```

Start monitor:

```bash
./cloudshell-mc-autostart -mode start
```

Stop monitor:

```bash
./cloudshell-mc-autostart -mode stop
```

Apply RCON/query config again:

```bash
./cloudshell-mc-autostart -mode configure
```

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

The monitor keeps roughly 30 days of samples and prunes older entries.

## Rerunning The Setup Script

If `~/minecraft-server` already exists and is non-empty, the script refuses to continue by default.

Reuse the existing directory:

```bash
./setup-minecraft-cloudshell.sh --reuse --agree-eula
```

Move the existing directory aside and create a fresh one:

```bash
./setup-minecraft-cloudshell.sh --force --agree-eula
```

The `.bashrc` autostart block is idempotent. Rerunning the script replaces the old managed block instead of appending duplicates.

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
