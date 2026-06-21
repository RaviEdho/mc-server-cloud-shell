# Current Behavior Checklist

This checklist captures the behavior that should remain intact while Phase 1 and Phase 2 reshape the installer.

## Installer Entry Points

- `install.sh` is the new public entry point.
- `setup-minecraft-cloudshell.sh` remains available during the rewrite, but new docs should prefer `install.sh`.
- The default one-liner is:

```bash
curl -fsSL https://raw.githubusercontent.com/RaviEdho/mc-server-cloud-shell/master/install.sh | bash
```

## Cloud Shell Install Path

The Cloud Shell-compatible install path should still:

- Install into `~/minecraft-server` by default.
- Resolve the requested Minecraft version from Mojang metadata.
- Resolve the required Java major version from Mojang metadata, with the existing fallback table.
- Install/select Java through SDKMAN.
- Download the Fabric installer.
- Install the Fabric server files.
- Prompt for Minecraft EULA acceptance unless `--agree-eula` is provided.
- Download `playit-linux-amd64` and `playit-cli-linux-amd64`.
- Run the interactive playit claim flow unless `--skip-playit-claim` is provided.
- Build the current Go monitor.
- Configure RCON/query in `server.properties`.
- Add the managed `.bashrc` autostart block.
- Start the monitor unless `--no-start` is provided.
- Verify the monitor and Minecraft server become reachable.

## One-Line Installer Behavior

The one-line install path should:

- Read prompts from `/dev/tty`, not stdin.
- Download the monitor source when the installer is run from stdin.
- Clean temporary downloaded files after setup exits.
- Keep setup-owned output colored separately from dependency output when the terminal supports color.
- Support `SETUP_NO_COLOR=1` to disable setup output coloring.

## Supported Options

The current installer behavior should continue to support:

```text
--mc-version VERSION
--java-major VERSION
--install-dir DIR
--agree-eula
--reuse
--force
--skip-playit-claim
--no-start
--update-monitor
--update-autostart
--platform auto|cloudshell|generic-linux
--service auto|bashrc|systemd-user|none
-h, --help
```

`--platform generic-linux`, `--service systemd-user`, and `--service none` are supported by the Phase 4 service path. Generic Linux uses `systemd --user` when available and installs `start.sh`, `stop.sh`, and `status.sh` as manual fallbacks.

## Dashboard And Monitor

The current Go monitor should still:

- Run as `~/minecraft-server/cloudshell-mc-monitor`.
- Expose the dashboard/API on port `8080`.
- Supervise Minecraft and playit.
- Report machine, Minecraft, and playit status.
- Tail/search logs.
- Accept RCON commands through the dashboard.
- Start, stop, restart, and kill Minecraft/playit from the dashboard.

## Verification Commands

Minimum checks for Phase 1 and Phase 2:

```bash
bash -n install.sh
cat install.sh | bash -s -- --help
cat install.sh | bash -s -- --bad-option
cat setup-minecraft-cloudshell.sh | bash -s -- --help
```
