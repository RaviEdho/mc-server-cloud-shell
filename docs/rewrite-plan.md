# Rewrite Plan

This plan turns the current Cloud Shell-specific installer into a lower-friction Minecraft server installer that can also run on a normal headless Linux VM. The main goal is to reduce install friction without losing the current one-command setup, playit tunnel support, monitor, and dashboard.

## Goals

- Keep a one-line install command as the primary entry point.
- Leave only `~/minecraft-server` as the visible install output for the default user flow.
- Support Google Cloud Shell as a first-class target.
- Add Ubuntu/Debian headless VM support that does not require opening inbound Minecraft ports.
- Replace the Go monitor with a Python 3 monitor that reaches full feature parity with the current Go setup.
- Keep dependency output visually distinct from setup output.
- Preserve explicit Minecraft EULA handling.
- Make the installer safe to rerun, update, and partially recover.

## Non-Goals

- Do not build a full cloud provisioner.
- Do not support every Linux distribution in the first rewrite.
- Do not require users to understand firewall, port forwarding, or reverse proxy setup for the default path.
- Do not silently accept the Minecraft EULA.
- Do not keep the Go monitor as a long-term compatibility path.
- Do not make system-wide changes on generic Linux unless the user explicitly chooses them or they are required for service management.

## Current State

The repository currently has three main files:

- `setup-minecraft-cloudshell.sh`: Bash installer for Google Cloud Shell.
- `cloudshell-mc-monitor.go`: Go supervisor, dashboard, metrics collector, process manager, and RCON helper.
- `README.md`: user-facing installation and behavior docs.

The installer currently:

- Installs Java through SDKMAN.
- Resolves Minecraft and Fabric versions.
- Downloads and installs a Fabric server.
- Downloads playit binaries.
- Runs the interactive playit claim flow.
- Builds the Go monitor.
- Writes RCON/query settings.
- Adds a Cloud Shell `.bashrc` autostart hook.
- Starts the monitor and verifies the server.

The main Cloud Shell assumptions are:

- `$HOME` is the install root.
- `.bashrc` is the autostart mechanism.
- Dashboard access is through Cloud Shell Web Preview on port `8080`.
- SDKMAN is acceptable for Java installation.
- Go is available or acceptable to install as a required build dependency.

## Product Decisions

These decisions define the first rewrite target:

- Generic Linux support starts with Ubuntu/Debian only.
- Java installation prefers `apt`/package-manager installation on normal Linux VMs.
- SDKMAN is a fallback for cases where package-manager installation is not suitable, including Google Cloud Shell-style environments where system installs are not persistent.
- Generic Linux defaults to `systemd --user` services.
- Manual `start.sh`, `stop.sh`, and `status.sh` scripts are still installed as a fallback.
- The monitor dashboard should bind to `127.0.0.1:8080` by default on generic Linux.
- Public dashboard exposure must be opt-in and should require dashboard authentication first.
- The Python monitor should reach full parity with the current Go monitor before it replaces the Go path.
- Public names should become generic, for example `install.sh` and `mc-monitor`, while still supporting Cloud Shell.
- The old `setup-minecraft-cloudshell.sh` does not need to remain as a compatibility wrapper.
- The Go monitor should be removed as soon as the Python monitor can replace it.

## Target Architecture

The public installer should stay as a single-file `install.sh` first. This keeps the one-line install simple and avoids a bootstrapper that has to download helper scripts before doing real work.

Internally, the installer should still be organized into clear function sections:

```text
install.sh
├── logging/output
├── argument parsing
├── prerequisite checks
├── platform detection
├── Cloud Shell platform behavior
├── Ubuntu/Debian platform behavior
├── minecraft installation
├── java installation
├── playit installation
├── monitor installation
├── service/autostart setup
├── verification
└── update/repair commands

monitor.py
├── supervisor
├── Minecraft process control
├── playit process control
├── health checks
├── metrics
├── RCON helper
└── dashboard/API
```

The installer should remain Bash because it is the most likely shell environment on target systems. The monitor should move toward Python 3 if the priority is avoiding Go as an installation dependency.

Splitting the installer source into multiple files can be reconsidered later, but only if the release process bundles those files back into a single downloadable `install.sh`. The default one-liner should not need to clone the repo or download a tree of helper scripts.

## Proposed File Layout

Repository layout:

```text
README.md
install.sh
monitor/
  monitor.py
  dashboard/
docs/
  rewrite-plan.md
```

Optional future source layout if the installer grows too large:

```text
installer-src/
  install.sh
  lib/
    logging.sh
    platform.sh
    minecraft.sh
    java.sh
    playit.sh
    monitor.sh
    systemd.sh
scripts/
  bundle-installer.sh
install.sh
```

In that future layout, `install.sh` remains the generated single-file installer users download.

Installed layout:

```text
~/minecraft-server/
  server.jar
  fabric-server-launch.jar
  server.properties
  eula.txt
  playit-linux-amd64
  playit-cli-linux-amd64
  monitor.py
  start.sh
  stop.sh
  status.sh
  .runtime/
  .monitor/
  .setup/
```

On generic Linux, service files may also be installed outside the server directory if systemd is selected:

```text
~/.config/systemd/user/minecraft-monitor.service
```

## Platform Strategy

### Google Cloud Shell

Keep Cloud Shell behavior close to what exists today:

- Default install directory: `~/minecraft-server`.
- Autostart through a managed `.bashrc` block.
- Dashboard hint: Cloud Shell Web Preview on port `8080`.
- Use playit to expose Minecraft without opening inbound ports.
- Keep cleanup behavior for one-line installation.

### Generic Headless Linux VM

Generic Linux should support:

- Debian/Ubuntu first.
- User-level install by default.
- playit tunnel by default, so no inbound port opening is required.
- `systemd --user` service if available.
- Foreground/manual scripts if systemd is unavailable.

The generic Linux path should avoid assuming:

- Cloud Shell Web Preview exists.
- `.bashrc` autostart is desired.
- SDKMAN is the best Java provider.
- The VM has Go installed.

Other Linux distributions can be considered after the Debian/Ubuntu path is stable.

## Runtime Choice

Recommended default:

- Bash for installation.
- Python 3 for the monitor.
- No required Python packages outside the standard library.

Why Python 3:

- More commonly installed on Linux VMs than Go.
- Suitable for HTTP dashboard, JSON API, subprocess supervision, log tailing, sockets, metrics, and RCON.
- Avoids compiling a monitor during install.
- Keeps the one-line installer simpler.

Fallback behavior:

- If Python 3 is missing, the installer should explain the missing dependency and offer distro-specific install guidance.
- The installer should not silently install Python unless the platform adapter explicitly supports package installation and the user chooses that path.

Java install behavior:

- On Ubuntu/Debian, prefer installing the required Java version through `apt` when available.
- On Cloud Shell or package-manager edge cases, use SDKMAN as the fallback Java provider.
- Keep `--java-major` so users can override version resolution.

## Installation UX

Default one-liner:

```bash
curl -fsSL https://raw.githubusercontent.com/RaviEdho/mc-server-cloud-shell/master/install.sh | bash
```

Expected default behavior:

- Prompt for EULA unless `--agree-eula` is passed.
- Download any required repo files into temporary files.
- Clean temporary installer files after setup.
- Leave `~/minecraft-server` as the only visible project directory in the home directory.
- Use playit so the user does not need to open inbound ports.

Useful options to preserve or add:

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
--platform cloudshell|generic-linux|auto
--service auto|systemd-user|bashrc|none
```

The old `setup-minecraft-cloudshell.sh` name can be removed during the rewrite. The new entry point should be generic, with Cloud Shell handled as a supported platform rather than as the product name.

## Service Management

The monitor should manage Minecraft and playit as child processes, as it does today.

Platform startup should be separate:

- Cloud Shell: `.bashrc` starts the monitor.
- Generic Linux with systemd: `systemd --user` starts the monitor.
- Generic Linux without systemd: installed `start.sh`, `stop.sh`, and `status.sh` scripts control the monitor manually.

The first generic Linux implementation should prefer user-level systemd over root-level systemd to reduce permission prompts and avoid system-wide writes.

## Dashboard Access

Cloud Shell can expose the dashboard through the built-in Web Preview on port `8080`. Generic Linux does not have an equivalent built-in preview, and the installer should expose as little network surface as possible.

Default dashboard behavior:

- Bind the monitor dashboard to `127.0.0.1:8080` on generic Linux.
- Do not listen on `0.0.0.0` by default.
- Do not open firewall ports for the dashboard.
- Do not create a public tunnel for the dashboard by default.
- Add dashboard authentication before supporting public dashboard exposure.

Recommended access methods:

```text
local      Default. Dashboard is reachable only from the VM itself.
ssh        Recommended for users who already SSH into the VM.
tailscale  Optional private preview through the user's tailnet.
cloudflare Optional authenticated browser access through Cloudflare Tunnel and Access.
playit     Optional only after dashboard authentication exists.
public     Not recommended without explicit opt-in and authentication.
```

The first generic Linux release should document SSH local port forwarding as the primary access path:

```bash
ssh -L 8080:127.0.0.1:8080 user@server
```

Then the user opens:

```text
http://127.0.0.1:8080
```

This keeps the dashboard private and reuses the existing SSH connection instead of opening another inbound port.

Tailscale Serve is a strong optional "web preview" candidate because it can share the dashboard inside the user's private tailnet. Cloudflare Tunnel is a stronger candidate for browser access from anywhere, but it should be paired with Cloudflare Access or equivalent authentication. playit should remain focused on Minecraft traffic unless the dashboard has authentication.

## Migration Phases

### Phase 1: Document and Freeze Current Behavior

- Keep the existing installer and Go monitor working.
- Add this rewrite plan.
- Add a short current-behavior checklist to `docs/current-behavior.md`.
- Decide what counts as a successful install for Cloud Shell and generic Linux.

Exit criteria:

- The desired behavior is written down.
- Open decisions are answered enough to start refactoring.
- No runtime behavior changes are required in this phase.

### Phase 2: Create The New Single-File Installer Skeleton

- Introduce `install.sh` as the new entry point.
- Keep `install.sh` as a single public file.
- Split installer code into clearer function sections:
  - logging
  - prerequisites
  - Java
  - Minecraft/Fabric
  - playit
  - monitor
  - autostart
  - verification
- Do not download helper scripts at runtime.
- Do not require cloning the repository for one-line install.
- Keep behavior close to the current Cloud Shell installer while using generic naming.
- Do not preserve `setup-minecraft-cloudshell.sh` as a required wrapper.

Exit criteria:

- Existing Cloud Shell install path still works.
- The one-line installer remains `curl .../install.sh | bash`.
- `bash -n` passes.
- Help output still works through stdin, for example `cat install.sh | bash -s -- --help`.

### Phase 3: Add Platform Detection

- Add `detect_platform`.
- Return at least:
  - `cloudshell`
  - `generic-linux`
  - `unknown-linux`
- Add `--platform` override.
- Move Cloud Shell-specific behavior behind platform functions.

Exit criteria:

- Cloud Shell behavior is unchanged when detected.
- Generic Linux can be detected without changing install behavior yet.
- Unsupported platforms fail with a clear message.

### Phase 4: Add Generic Linux Service Support

- Add `systemd --user` support.
- Add manual `start.sh`, `stop.sh`, and `status.sh` scripts.
- Keep `.bashrc` autostart limited to Cloud Shell unless explicitly requested.
- Keep playit as the default exposure path.

Exit criteria:

- A generic Linux VM can start the monitor after login through systemd user services when available.
- A VM without systemd can still start manually.
- No inbound Minecraft port is required when playit is claimed.

### Phase 5: Replace The Go Monitor With A Python Monitor

- Implement `monitor/monitor.py` with the same core CLI surface:
  - `-start`
  - `-stop`
  - `-status`
  - `-configure`
  - restart monitor/Minecraft/playit
- Match the existing dashboard/API behavior before making Python the default.
- Use the current Go monitor as the behavior reference, not as a long-term fallback.

Exit criteria:

- Python monitor can start Minecraft and playit.
- Python monitor can expose the dashboard on port `8080`.
- Python monitor can read status and logs.
- Python monitor can stop Minecraft gracefully through RCON.
- Python dashboard reaches full parity with the current Go dashboard.

### Phase 6: Remove The Go Path

- Make Python the only default monitor runtime.
- Remove Go from default prerequisites.
- Remove Go build logic from the installer.
- Remove Go monitor source if no longer needed.
- Remove Go-specific docs and cleanup paths.
- Update README examples and troubleshooting.

Exit criteria:

- Default one-line install does not require Go.
- Cloud Shell install works with Python monitor.
- Generic Linux install works with Python monitor.
- No default path downloads, builds, or mentions Go.
- Existing installations can update without leaving broken legacy files.

## Testing Plan

Minimum local checks:

```bash
bash -n install.sh
cat install.sh | bash -s -- --help
cat install.sh | bash -s -- --bad-option
```

Installer behavior checks:

- One-line install can read prompts from `/dev/tty`.
- Temporary files are cleaned on success and failure.
- Missing commands produce clear errors.
- `--install-dir`, `--reuse`, and `--force` behave safely.
- `--update-monitor` does not reinstall the whole server.

Platform checks:

- Cloud Shell detection works in Cloud Shell.
- Generic Linux detection works on a normal VM.
- `--platform` override works.
- Unknown Linux fails clearly.

Monitor checks:

- Starts Minecraft.
- Starts playit.
- Stops Minecraft gracefully through RCON.
- Restarts Minecraft.
- Reports status.
- Serves dashboard/API.
- Handles stale pid files.
- Handles already-running processes.
- Writes useful logs.

## Risks

- Rewriting the monitor can accidentally drop dashboard features.
- systemd user services can behave differently across distros and login/session setups.
- Java installation strategy differs by platform.
- playit first-time setup is interactive and can be hard to automate in tests.
- Cloud Shell lifecycle limitations still apply; the server cannot run while the Cloud Shell VM is stopped.
- Supporting too many Linux distributions at once can stall the rewrite.

## Open Decisions

These are now decided:

1. Generic Linux starts with Ubuntu/Debian only.
2. Java uses the package manager first on generic Linux, with SDKMAN as fallback.
3. Generic Linux defaults to `systemd --user`, with manual scripts available.
4. The Python monitor should reach full feature parity with the Go dashboard.
5. Public names become generic while still supporting Cloud Shell.
6. The old Cloud Shell script name does not need a compatibility wrapper.
7. The Go monitor should be removed as soon as Python can replace it.
8. The generic Linux dashboard binds to `127.0.0.1:8080` by default.
9. SSH local port forwarding is the primary documented dashboard access path for generic Linux.
10. Public dashboard exposure is opt-in only and should require authentication first.
11. Generic Linux Java installation should use `sudo apt-get install` automatically when the required package is available.
12. Rewrite work continues on the `rewrite-generic-installer` branch.

Remaining decisions before implementation:

1. Which Ubuntu/Debian releases are the first supported targets?
2. Should playit be mandatory for the default install, or can users choose local-only setup?

## Recommended Starting Point

Start with Phase 1 and Phase 2 only.

The first implementation change should not add generic Linux support or Python yet. It should create the new generic entry point and reshape the installer so the current behavior is easier to move:

- Add `install.sh` as the future entry point.
- Keep Cloud Shell behavior working through the new generic installer.
- Extract platform-specific assumptions into named functions.
- Keep the public installer single-file.
- Add no new runtime dependencies.

That gives the rewrite a stable base and keeps every future change smaller.
