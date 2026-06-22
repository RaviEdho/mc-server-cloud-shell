#!/usr/bin/env python3
import argparse
import contextlib
from email import policy
from email.parser import BytesParser
import json
import os
import re
import secrets
import shutil
import signal
import socket
import struct
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import zipfile
from dataclasses import dataclass
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse


LOCK_FILE = "supervisor.lock"
PID_FILE = "supervisor.pid"
METRIC_RETENTION_SEC = 7 * 24 * 60 * 60


def now_iso():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def env_default(name, fallback):
    return os.environ.get(name) or fallback


def file_exists(path):
    return Path(path).is_file()


def process_running(pid):
    if pid <= 0:
        return False
    status = Path("/proc") / str(pid) / "status"
    if status.exists():
        with contextlib.suppress(OSError):
            for line in status.read_text(errors="ignore").splitlines():
                if line.startswith("State:") and "Z" in line:
                    return False
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def wait_for_exit(pid, timeout):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if not process_running(pid):
            return True
        time.sleep(0.25)
    return not process_running(pid)


def read_pid(path):
    try:
        return int(Path(path).read_text().strip())
    except (OSError, ValueError):
        return 0


def write_pid(path, pid):
    Path(path).write_text(f"{pid}\n")


def read_or_create_secret(path):
    path = Path(path)
    with contextlib.suppress(OSError):
        value = path.read_text().strip()
        if value:
            return value
    path.parent.mkdir(parents=True, exist_ok=True)
    value = secrets.token_hex(18)
    path.write_text(value + "\n")
    with contextlib.suppress(OSError):
        path.chmod(0o600)
    return value


def parse_properties_text(text):
    props = {}
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        props[key] = value
    return props


def parse_properties(path):
    try:
        return parse_properties_text(Path(path).read_text(errors="ignore"))
    except OSError:
        return {}


def write_properties(path, updates):
    path = Path(path)
    original = path.read_text(errors="ignore")
    seen = set()
    lines = []
    for line in original.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#") or "=" not in line:
            lines.append(line)
            continue
        key, _ = line.split("=", 1)
        if key in updates:
            lines.append(f"{key}={updates[key]}")
            seen.add(key)
        else:
            lines.append(line)
    for key in sorted(set(updates) - seen):
        lines.append(f"{key}={updates[key]}")
    path.write_text("\n".join(lines) + "\n")


def format_bytes(size):
    if size <= 0:
        return "0 B"
    units = ["B", "KB", "MB", "GB", "TB"]
    value = float(size)
    idx = 0
    while value >= 1024 and idx < len(units) - 1:
        value /= 1024
        idx += 1
    if idx == 0:
        return f"{int(value)} {units[idx]}"
    return f"{value:.1f} {units[idx]}"


def dir_size(path):
    total = 0
    path = Path(path)
    if not path.exists():
        return 0
    for root, dirs, files in os.walk(path):
        dirs[:] = [d for d in dirs if not (Path(root) / d).is_symlink()]
        for name in files:
            file_path = Path(root) / name
            if file_path.is_symlink():
                continue
            with contextlib.suppress(OSError):
                total += file_path.stat().st_size
    return total


def round1(value):
    return round(value, 1)


@dataclass
class Config:
    root: Path
    runtime_dir: Path
    monitor_dir: Path
    server_dir: Path
    java_bin: str
    min_ram: str
    max_ram: str
    socket_path: str
    secret_path: Path
    web_addr: str
    rcon_host: str
    rcon_port: str
    rcon_pass: str
    health_interval: float


def load_config(create_secret=True):
    exe = Path(sys.argv[0]).resolve()
    root = os.environ.get("MC_MONITOR_ROOT") or os.environ.get("MC_AUTOSTART_ROOT") or str(exe.parent)
    root = Path(root).expanduser().resolve()
    home = Path.home()

    java_bin = os.environ.get("JAVA_BIN")
    if not java_bin:
        sdkman_java = home / ".sdkman" / "candidates" / "java" / "current" / "bin" / "java"
        java_bin = str(sdkman_java) if sdkman_java.exists() else "java"

    server_dir = os.environ.get("MC_SERVER_DIR")
    if not server_dir:
        server_dir = str(root if (root / "fabric-server-launch.jar").exists() else root / "server")
    server_dir = Path(server_dir).expanduser().resolve()
    runtime_dir = root / ".runtime"
    rcon_pass = os.environ.get("MC_RCON_PASSWORD")
    if not rcon_pass:
        secret_path = runtime_dir / "rcon.password"
        if create_secret:
            rcon_pass = read_or_create_secret(secret_path)
        else:
            with contextlib.suppress(OSError):
                rcon_pass = secret_path.read_text().strip()
    rcon_pass = rcon_pass or ""

    return Config(
        root=root,
        runtime_dir=runtime_dir,
        monitor_dir=root / ".monitor",
        server_dir=server_dir,
        java_bin=java_bin,
        min_ram=env_default("MC_MIN_RAM", "1G"),
        max_ram=env_default("MC_MAX_RAM", "2G"),
        socket_path=env_default("PLAYIT_SOCKET", "/tmp/playit.sock"),
        secret_path=Path(env_default("PLAYIT_SECRET", str(home / ".config" / "playit_gg" / "playit.toml"))),
        web_addr=env_default("MC_MONITOR_ADDR", "0.0.0.0:8080"),
        rcon_host=env_default("MC_RCON_HOST", "127.0.0.1"),
        rcon_port=env_default("MC_RCON_PORT", "25575"),
        rcon_pass=rcon_pass,
        health_interval=float(env_default("MC_HEALTH_INTERVAL_SECONDS", "30")),
    )


def ensure_server_properties(cfg):
    path = cfg.server_dir / "server.properties"
    if not path.exists():
        raise RuntimeError(f"missing {path}")
    write_properties(path, {
        "enable-rcon": "true",
        "rcon.port": cfg.rcon_port,
        "rcon.password": cfg.rcon_pass,
        "enable-query": "true",
        "query.port": parse_properties(path).get("query.port") or "25565",
    })


def validate_config(cfg):
    missing = []
    if not (cfg.server_dir / "fabric-server-launch.jar").exists():
        missing.append(str(cfg.server_dir / "fabric-server-launch.jar"))
    if not (cfg.root / "playit-linux-amd64").exists():
        missing.append(str(cfg.root / "playit-linux-amd64"))
    if not cfg.secret_path.exists():
        missing.append(f"playit secret at {cfg.secret_path}; run playit CLI setup first")
    if os.sep in cfg.java_bin and not Path(cfg.java_bin).exists():
        missing.append(f"Java binary at {cfg.java_bin}")
    if missing:
        raise RuntimeError("missing " + ", ".join(missing))


class ProcessManager:
    def __init__(self, name, cwd, args, adopt_tokens, before_start=None):
        self.name = name
        self.cwd = Path(cwd)
        self.args = args
        self.adopt_tokens = adopt_tokens
        self.before_start = before_start
        self.desired = False
        self.running = False
        self.pid = 0
        self.started_at = None
        self.stopped_at = None
        self.last_error = ""
        self.proc = None
        self.adopted = False
        self.lock = threading.Lock()

    def start_desired(self):
        with self.lock:
            self.desired = True

    def stop_desired(self):
        with self.lock:
            self.desired = False

    def snapshot(self):
        with self.lock:
            started = self.started_at
            stopped = self.stopped_at
            running = self.running
            return {
                "desired": self.desired,
                "running": running,
                "pid": self.pid,
                "startedAt": started or "",
                "stoppedAt": stopped or "",
                "lastError": self.last_error,
                "uptimeSec": int(time.time() - parse_iso_timestamp(started)) if running and started else 0,
            }

    def is_desired(self):
        with self.lock:
            return self.desired

    def mark_started(self, proc, adopted=False):
        with self.lock:
            self.proc = None if adopted else proc
            self.pid = proc if isinstance(proc, int) else proc.pid
            self.running = True
            self.adopted = adopted
            if not self.started_at:
                self.started_at = now_iso()
            self.stopped_at = ""
            self.last_error = ""

    def mark_stopped(self, err=None):
        with self.lock:
            self.proc = None
            self.pid = 0
            self.running = False
            self.adopted = False
            self.stopped_at = now_iso()
            if err:
                self.last_error = str(err)

    def set_error(self, err):
        with self.lock:
            self.last_error = str(err)

    def pid_path(self, runtime_dir):
        return Path(runtime_dir) / f"{self.name}.pid"

    def find_adoptable_pid(self, runtime_dir):
        pid = read_pid(self.pid_path(runtime_dir))
        if process_running(pid):
            return pid
        pid = find_process_by_cmdline(self.adopt_tokens)
        if pid:
            return pid
        with contextlib.suppress(OSError):
            self.pid_path(runtime_dir).unlink()
        return 0

    def supervise(self, stop_event, runtime_dir, handoff_event, log):
        backoff = 5
        while not stop_event.is_set():
            if not self.is_desired():
                stop_event.wait(1)
                continue
            pid = self.find_adoptable_pid(runtime_dir)
            if pid:
                self.mark_started(pid, adopted=True)
                write_pid(self.pid_path(runtime_dir), pid)
                log(f"adopted existing {self.name} process group {pid}")
                self.monitor_adopted(stop_event, runtime_dir, log)
                continue
            if self.before_start:
                self.before_start()
            log_path = Path(runtime_dir) / f"{self.name}.log"
            try:
                out = log_path.open("ab")
                log(f"starting {self.name}: {' '.join(self.args)}")
                proc = subprocess.Popen(
                    self.args,
                    cwd=str(self.cwd),
                    stdin=subprocess.DEVNULL,
                    stdout=out,
                    stderr=out,
                    start_new_session=True,
                    env=os.environ.copy(),
                )
                write_pid(self.pid_path(runtime_dir), proc.pid)
                self.mark_started(proc)
                while proc.poll() is None and not stop_event.is_set():
                    time.sleep(0.5)
                if stop_event.is_set() and handoff_event.is_set():
                    out.close()
                    return
                if proc.poll() is None:
                    log(f"stopping {self.name}")
                    stop_process_group(proc.pid, 10, log)
                    proc.wait(timeout=15)
                code = proc.poll()
                out.close()
                self.mark_stopped(RuntimeError(f"exit code {code}") if code else None)
                with contextlib.suppress(OSError):
                    self.pid_path(runtime_dir).unlink()
                log(f"{self.name} exited with code {code}")
            except Exception as exc:
                self.set_error(exc)
                log(f"{self.name} failed: {exc}")
            if self.is_desired() and not stop_event.is_set():
                log(f"restarting {self.name} in {backoff}s")
                stop_event.wait(backoff)
                backoff = min(backoff * 2, 60)

    def monitor_adopted(self, stop_event, runtime_dir, log):
        while not stop_event.is_set():
            with self.lock:
                pid = self.pid
                desired = self.desired
            if pid <= 0:
                return
            if not desired:
                log(f"stopping adopted {self.name} process group {pid}")
                stop_process_group(pid, 10, log)
            if not desired or not process_running(pid):
                self.mark_stopped()
                with contextlib.suppress(OSError):
                    self.pid_path(runtime_dir).unlink()
                return
            stop_event.wait(1)

    def stop_gracefully(self, timeout, log):
        self.stop_desired()
        with self.lock:
            pid = self.pid
            proc = self.proc
        if proc and proc.pid:
            pid = proc.pid
        if pid > 0:
            stop_process_group(pid, timeout, log)

    def kill(self, log):
        self.stop_desired()
        with self.lock:
            pid = self.pid
        if pid > 0:
            with contextlib.suppress(OSError):
                os.killpg(pid, signal.SIGKILL)
            log(f"killed {self.name} process group {pid}")


def parse_iso_timestamp(value):
    if not value:
        return 0
    with contextlib.suppress(ValueError):
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    return 0


def stop_process_group(pid, timeout, log):
    if pid <= 0:
        return
    try:
        os.killpg(pid, signal.SIGTERM)
    except OSError as exc:
        log(f"SIGTERM failed for process group {pid}: {exc}")
        return
    deadline = time.time() + timeout
    while time.time() < deadline:
        if not process_running(pid):
            return
        time.sleep(0.5)
    with contextlib.suppress(OSError):
        os.killpg(pid, signal.SIGKILL)


def find_process_by_cmdline(tokens):
    if not tokens or not Path("/proc").exists():
        return 0
    self_pid = os.getpid()
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        pid = int(entry.name)
        if pid <= 0 or pid == self_pid:
            continue
        try:
            cmdline = (entry / "cmdline").read_bytes().replace(b"\x00", b" ").decode(errors="ignore")
        except OSError:
            continue
        if all(token in cmdline for token in tokens if token) and process_running(pid):
            return pid
    return 0


class RCONClient:
    def __init__(self, host, port, password):
        self.addr = (host, int(port))
        self.password = password
        self.sock = None
        self.next_id = 10
        self.lock = threading.Lock()

    def close(self):
        with self.lock:
            self._close_locked()

    def command(self, command):
        result = self.commands(command)
        return result[0] if result else ""

    def commands(self, *commands):
        with self.lock:
            self._ensure_locked()
            try:
                return self._commands_locked(*commands)
            except OSError:
                self._close_locked()
                self._ensure_locked()
                return self._commands_locked(*commands)

    def _ensure_locked(self):
        if self.sock:
            return
        sock = socket.create_connection(self.addr, timeout=2)
        sock.settimeout(4)
        req_id = self._next_locked()
        self._write(sock, req_id, 3, self.password)
        resp_id, _, _ = self._read(sock)
        if resp_id == -1:
            sock.close()
            raise RuntimeError("rcon authentication failed")
        self.sock = sock

    def _commands_locked(self, *commands):
        output = []
        for command in commands:
            req_id = self._next_locked()
            self._write(self.sock, req_id, 2, command)
            resp_id, _, body = self._read(self.sock)
            if resp_id != req_id:
                raise RuntimeError(f"unexpected rcon response id {resp_id} for request {req_id}")
            output.append(body)
        return output

    def _next_locked(self):
        self.next_id += 1
        return self.next_id

    def _close_locked(self):
        if self.sock:
            with contextlib.suppress(OSError):
                self.sock.close()
            self.sock = None

    @staticmethod
    def _write(sock, req_id, typ, body):
        payload = body.encode()
        packet = struct.pack("<iii", 4 + 4 + len(payload) + 2, req_id, typ) + payload + b"\x00\x00"
        sock.sendall(packet)

    @staticmethod
    def _read(sock):
        header = read_exact(sock, 4)
        (length,) = struct.unpack("<i", header)
        if length < 10 or length > 4096:
            raise RuntimeError(f"invalid rcon packet length {length}")
        data = read_exact(sock, length)
        req_id, typ = struct.unpack("<ii", data[:8])
        body = data[8:].rstrip(b"\x00").decode(errors="replace")
        return req_id, typ, body


def read_exact(sock, n):
    chunks = []
    remaining = n
    while remaining:
        chunk = sock.recv(remaining)
        if not chunk:
            raise OSError("unexpected EOF")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


class MetricStore:
    def __init__(self, path):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.lock = threading.Lock()
        self.points = []
        with contextlib.suppress(OSError):
            for line in self.path.read_text(errors="ignore").splitlines():
                with contextlib.suppress(json.JSONDecodeError):
                    self.points.append(json.loads(line))

    def add(self, point):
        with self.lock:
            self.points.append(point)
            with self.path.open("a") as f:
                f.write(json.dumps(point) + "\n")

    def since(self, max_age):
        cutoff = time.time() - max_age
        with self.lock:
            return [p for p in self.points if parse_iso_timestamp(p.get("time", "")) >= cutoff]

    def prune(self, max_age):
        cutoff = time.time() - max_age
        with self.lock:
            self.points = [p for p in self.points if parse_iso_timestamp(p.get("time", "")) >= cutoff]
            self.path.write_text("".join(json.dumps(p) + "\n" for p in self.points))


class App:
    def __init__(self, cfg, log):
        self.cfg = cfg
        self.log = log
        self.metrics = MetricStore(cfg.monitor_dir / "metrics.jsonl")
        self.rcon = RCONClient(cfg.rcon_host, cfg.rcon_port, cfg.rcon_pass)
        self.minecraft_cache = None
        self.cache_lock = threading.Lock()
        self.cpu_last = None
        self.stop_event = threading.Event()
        self.handoff_event = threading.Event()
        self.playit = ProcessManager(
            "playit",
            cfg.root,
            [str(cfg.root / "playit-linux-amd64"), "--socket-path", cfg.socket_path, "--secret-path", str(cfg.secret_path)],
            ["playit-linux-amd64", "--socket-path", cfg.socket_path],
            before_start=lambda: Path(cfg.socket_path).unlink(missing_ok=True),
        )
        self.minecraft = ProcessManager(
            "minecraft",
            cfg.server_dir,
            [cfg.java_bin, "-Xms" + cfg.min_ram, "-Xmx" + cfg.max_ram, "-jar", "fabric-server-launch.jar", "nogui"],
            ["fabric-server-launch.jar", "nogui"],
        )

    def run(self):
        validate_config(self.cfg)
        ensure_server_properties(self.cfg)
        self.playit.start_desired()
        self.minecraft.start_desired()
        threads = [
            threading.Thread(target=self.playit.supervise, args=(self.stop_event, self.cfg.runtime_dir, self.handoff_event, self.log), daemon=True),
            threading.Thread(target=self.minecraft.supervise, args=(self.stop_event, self.cfg.runtime_dir, self.handoff_event, self.log), daemon=True),
            threading.Thread(target=self.collect_metrics, daemon=True),
            threading.Thread(target=self.collect_health, daemon=True),
            threading.Thread(target=self.serve_http, daemon=True),
        ]
        self.log(f"monitor started root={self.cfg.root} web={self.cfg.web_addr} java={self.cfg.java_bin}")
        for thread in threads:
            thread.start()
        while not self.stop_event.is_set():
            time.sleep(0.5)
        self.log("monitor handoff requested" if self.handoff_event.is_set() else "monitor stopping")
        self.rcon.close()
        if not self.handoff_event.is_set():
            self.minecraft.stop_gracefully(30, self.log)
            self.playit.stop_gracefully(10, self.log)
            Path(self.cfg.socket_path).unlink(missing_ok=True)
        for thread in threads[:4]:
            thread.join(timeout=15)
        self.log("monitor stopped")

    def serve_http(self):
        host, port = split_host_port(self.cfg.web_addr)
        server = ThreadingHTTPServer((host, int(port)), lambda *args: Handler(self, *args))
        server.timeout = 1
        self.log(f"dashboard listening on http://{self.cfg.web_addr}")
        while not self.stop_event.is_set():
            server.handle_request()
        server.server_close()

    def status(self):
        machine = self.collect_machine_metric()
        web_host = os.environ.get("WEB_HOST", "")
        preview = ""
        if web_host:
            _, port = split_host_port(self.cfg.web_addr)
            preview = f"https://{port}-{web_host}"
        return {
            "generatedAt": now_iso(),
            "webAddr": self.cfg.web_addr,
            "previewHint": preview,
            "machine": machine,
            "minecraft": self.cached_minecraft_health(),
            "playit": self.playit_health(),
            "processes": {
                "minecraft": self.minecraft.snapshot(),
                "playit": self.playit.snapshot(),
            },
        }

    def cached_minecraft_health(self):
        with self.cache_lock:
            if self.minecraft_cache:
                return self.minecraft_cache
        return {
            "updatedAt": now_iso(),
            "portOpen": dial_ok(("127.0.0.1", 25565), 0.7),
            "rconOk": False,
            "version": parse_server_version(self.cfg.runtime_dir / "minecraft.log", self.cfg.server_dir / "logs" / "latest.log"),
            "world": read_world_info(self.cfg.server_dir / "server.properties"),
            "playersOnline": 0,
            "maxPlayers": read_max_players(self.cfg.server_dir / "server.properties"),
            "players": [],
            "tps": "",
            "mspt": "",
            "lastError": "waiting for first health sample",
        }

    def refresh_minecraft_health(self):
        props = self.cfg.server_dir / "server.properties"
        result = {
            "updatedAt": now_iso(),
            "portOpen": dial_ok(("127.0.0.1", 25565), 0.7),
            "rconOk": False,
            "version": parse_server_version(self.cfg.runtime_dir / "minecraft.log", self.cfg.server_dir / "logs" / "latest.log"),
            "world": read_world_info(props),
            "playersOnline": 0,
            "maxPlayers": read_max_players(props),
            "players": [],
            "tps": "",
            "mspt": "",
            "lastError": "",
        }
        try:
            outputs = self.rcon.commands("list", "tick query")
            result["rconOk"] = True
            online, max_players, players = parse_list_output(outputs[0] if outputs else "")
            result["playersOnline"] = online
            result["maxPlayers"] = max_players or result["maxPlayers"]
            result["players"] = players
            if len(outputs) > 1:
                tps, mspt = parse_tick_output(outputs[1])
                result["tps"] = tps
                result["mspt"] = mspt
        except Exception as exc:
            result["lastError"] = str(exc)
        with self.cache_lock:
            self.minecraft_cache = result

    def playit_health(self):
        return {
            "socketExists": Path(self.cfg.socket_path).exists(),
            "address": parse_playit_address(self.cfg.runtime_dir / "playit.log"),
            "lastError": "",
        }

    def collect_metrics(self):
        self.metrics.add(self.collect_machine_metric())
        while not self.stop_event.wait(30):
            self.metrics.add(self.collect_machine_metric())
            self.metrics.prune(METRIC_RETENTION_SEC)

    def collect_health(self):
        self.refresh_minecraft_health()
        while not self.stop_event.wait(self.cfg.health_interval):
            self.refresh_minecraft_health()

    def collect_machine_metric(self):
        cpu = self.cpu_percent()
        mem_used, mem_total = mem_usage()
        disk_used, disk_total = disk_usage(self.cfg.root)
        return {
            "time": now_iso(),
            "cpuPercent": round1(cpu),
            "memUsedMb": mem_used // 1024 // 1024,
            "memTotalMb": mem_total // 1024 // 1024,
            "memPercent": percent(mem_used, mem_total),
            "diskUsedMb": disk_used // 1024 // 1024,
            "diskTotalMb": disk_total // 1024 // 1024,
            "diskPercent": percent(disk_used, disk_total),
        }

    def cpu_percent(self):
        sample = read_cpu()
        if not sample:
            return 0.0
        if not self.cpu_last:
            self.cpu_last = sample
            return 0.0
        total_delta = sample[0] - self.cpu_last[0]
        idle_delta = sample[1] - self.cpu_last[1]
        self.cpu_last = sample
        return 0.0 if total_delta <= 0 else (total_delta - idle_delta) * 100.0 / total_delta

    def log_path(self, target):
        if target in ("server", "minecraft"):
            return self.cfg.runtime_dir / "minecraft.log"
        if target == "playit":
            return self.cfg.runtime_dir / "playit.log"
        if target == "latest":
            return self.cfg.server_dir / "logs" / "latest.log"
        return self.cfg.runtime_dir / "supervisor.log"

    def world_path(self):
        props_path = self.cfg.server_dir / "server.properties"
        world = read_world_info(props_path)
        name = world.get("name") or "world"
        candidate = Path(name)
        if candidate.is_absolute():
            raise RuntimeError("world name must be relative")
        target = (self.cfg.server_dir / candidate).resolve()
        server_dir = self.cfg.server_dir.resolve()
        if not str(target).startswith(str(server_dir)):
            raise RuntimeError("world path escapes server directory")
        return target, world


class Handler(BaseHTTPRequestHandler):
    def __init__(self, app, *args):
        self.app = app
        super().__init__(*args)

    def log_message(self, *_):
        return

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/":
            self.send_html(DASHBOARD_HTML)
        elif parsed.path == "/api/status":
            self.send_json(self.app.status())
        elif parsed.path == "/api/metrics":
            self.send_json(self.app.metrics.since(METRIC_RETENTION_SEC))
        elif parsed.path == "/api/logs":
            qs = parse_qs(parsed.query)
            target = qs.get("target", [""])[0]
            search = qs.get("q", [""])[0].strip()
            lines = tail_lines(self.app.log_path(target), 200, search)
            self.send_json({"target": target, "lines": lines})
        elif parsed.path == "/api/world/download":
            self.handle_world_download()
        else:
            self.send_error(HTTPStatus.NOT_FOUND)

    def do_POST(self):
        parsed = urlparse(self.path)
        if parsed.path == "/api/command":
            self.handle_command()
        elif parsed.path == "/api/world/upload":
            self.handle_world_upload()
        elif parsed.path.startswith("/api/minecraft/"):
            self.handle_action("minecraft", parsed.path.rsplit("/", 1)[-1])
        elif parsed.path.startswith("/api/playit/"):
            self.handle_action("playit", parsed.path.rsplit("/", 1)[-1])
        else:
            self.send_error(HTTPStatus.NOT_FOUND)

    def send_json(self, value, status=200):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def send_html(self, value):
        data = value.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def read_json(self, max_bytes=4096):
        length = min(int(self.headers.get("Content-Length", "0") or "0"), max_bytes)
        return json.loads(self.rfile.read(length) or b"{}")

    def handle_command(self):
        try:
            req = self.read_json()
            command = str(req.get("command", "")).strip().lstrip("/").strip()
            if not command:
                self.send_error(HTTPStatus.BAD_REQUEST, "command is required")
                return
            if len(command) > 1024:
                self.send_error(HTTPStatus.BAD_REQUEST, "command is too long")
                return
            rcon_command = dashboard_rcon_command(command)
            output = self.app.rcon.command(rcon_command)
            self.app.log(f"dashboard command: {command}" + (f" -> {rcon_command}" if rcon_command != command else ""))
            self.send_json({"command": command, "output": output})
        except Exception as exc:
            self.send_error(HTTPStatus.INTERNAL_SERVER_ERROR, str(exc))

    def handle_action(self, target, action):
        pm = self.app.minecraft if target == "minecraft" else self.app.playit
        if action == "start":
            pm.start_desired()
        elif action == "stop":
            if target == "minecraft":
                pm.stop_desired()
                with contextlib.suppress(Exception):
                    self.app.rcon.command("stop")
            else:
                pm.stop_gracefully(10, self.app.log)
        elif action == "restart":
            if target == "minecraft":
                pm.stop_desired()
                with contextlib.suppress(Exception):
                    self.app.rcon.command("stop")
            pm.stop_gracefully(20, self.app.log)
            pm.start_desired()
        elif action == "kill":
            pm.kill(self.app.log)
        else:
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        self.send_json({"ok": "true"})

    def handle_world_download(self):
        try:
            world_path, world = self.app.world_path()
            if not world_path.is_dir():
                self.send_error(HTTPStatus.NOT_FOUND, "world folder not found")
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/zip")
            self.send_header("Content-Disposition", f'attachment; filename="{safe_zip_name(world.get("name", "world"))}"')
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            with zipfile.ZipFile(self.wfile, "w", zipfile.ZIP_DEFLATED) as zf:
                zip_world(zf, world_path, world.get("name", "world"))
        except Exception as exc:
            self.send_error(HTTPStatus.BAD_REQUEST, str(exc))

    def handle_world_upload(self):
        try:
            if self.app.minecraft.snapshot().get("running"):
                self.send_error(HTTPStatus.CONFLICT, "stop Minecraft before uploading a world zip")
                return
            world_path, world = self.app.world_path()
            upload = self.read_multipart_file("world", max_bytes=2 << 30)
            if upload is None:
                self.send_error(HTTPStatus.BAD_REQUEST, "world zip file is required")
                return
            filename, file_data = upload
            if not filename.lower().endswith(".zip"):
                self.send_error(HTTPStatus.BAD_REQUEST, "only .zip files are accepted")
                return
            tmp_dir = self.app.cfg.runtime_dir / f"world-upload-{int(time.time())}-{secrets.token_hex(4)}"
            extract_dir = tmp_dir / "extract"
            zip_path = tmp_dir / "upload.zip"
            tmp_dir.mkdir(parents=True, exist_ok=False)
            try:
                zip_path.write_bytes(file_data)
                source = extract_world_zip(zip_path, extract_dir)
                replace_world_dir(source, world_path)
            finally:
                shutil.rmtree(tmp_dir, ignore_errors=True)
            self.app.log(f"world uploaded: {filename} -> {world.get('name', 'world')}")
            self.send_json({"ok": "true", "world": world.get("name", "world")})
        except Exception as exc:
            self.send_error(HTTPStatus.BAD_REQUEST, str(exc))

    def read_multipart_file(self, field_name, max_bytes):
        content_type = self.headers.get("Content-Type", "")
        if not content_type.lower().startswith("multipart/form-data"):
            return None
        length = int(self.headers.get("Content-Length", "0") or "0")
        if length <= 0 or length > max_bytes:
            raise RuntimeError("invalid zip upload")
        body = self.rfile.read(length)
        raw = (
            f"Content-Type: {content_type}\r\n"
            "MIME-Version: 1.0\r\n"
            "\r\n"
        ).encode() + body
        message = BytesParser(policy=policy.default).parsebytes(raw)
        if not message.is_multipart():
            return None
        for part in message.iter_parts():
            disposition = part.get_content_disposition()
            if disposition != "form-data":
                continue
            if part.get_param("name", header="content-disposition") != field_name:
                continue
            filename = part.get_filename() or ""
            return filename, part.get_payload(decode=True) or b""
        return None


def dashboard_rcon_command(command):
    parts = command.split(" ", 1)
    if len(parts) != 2 or parts[0].lower() != "say":
        return command
    message = parts[1].strip()
    if not message:
        return command
    return "tellraw @a " + json.dumps({"text": "[Server] " + message})


def tail_lines(path, limit, search=""):
    try:
        lines = Path(path).read_text(errors="ignore").replace("\r\n", "\n").split("\n")
    except OSError as exc:
        raise RuntimeError(str(exc))
    search_lower = search.lower()
    filtered = [line for line in lines if line.strip() and (not search_lower or search_lower in line.lower())]
    return filtered[-limit:]


def dial_ok(addr, timeout):
    try:
        with socket.create_connection(addr, timeout=timeout):
            return True
    except OSError:
        return False


def read_cpu():
    try:
        fields = Path("/proc/stat").read_text().splitlines()[0].split()
    except OSError:
        return None
    if not fields or fields[0] != "cpu":
        return None
    values = [int(v) for v in fields[1:]]
    total = sum(values)
    idle = values[3] + (values[4] if len(values) > 4 else 0)
    return total, idle


def mem_usage():
    vals = {}
    try:
        for line in Path("/proc/meminfo").read_text().splitlines():
            parts = line.split()
            if len(parts) >= 2:
                vals[parts[0].rstrip(":")] = int(parts[1]) * 1024
    except OSError:
        return 0, 0
    total = vals.get("MemTotal", 0)
    available = vals.get("MemAvailable", 0)
    return max(total - available, 0), total


def disk_usage(path):
    try:
        usage = shutil.disk_usage(path)
        return usage.used, usage.total
    except OSError:
        return 0, 0


def percent(used, total):
    return 0 if total == 0 else round1(used * 100.0 / total)


def read_world_info(props_path):
    props = parse_properties(props_path)
    server_dir = Path(props_path).parent
    name = props.get("level-name", "world").strip() or "world"
    size = dir_size(server_dir / name)
    return {
        "name": name,
        "gameMode": props.get("gamemode", "survival") or "survival",
        "difficulty": props.get("difficulty", "easy") or "easy",
        "hardcore": props.get("hardcore", "").lower() == "true",
        "sizeBytes": size,
        "size": format_bytes(size),
    }


def read_max_players(props_path):
    with contextlib.suppress(ValueError):
        return int(parse_properties(props_path).get("max-players", "0"))
    return 0


def parse_server_version(*paths):
    pattern = re.compile(r"Starting minecraft server version ([^ \n]+)")
    for path in paths:
        with contextlib.suppress(OSError):
            matches = pattern.findall(Path(path).read_text(errors="ignore"))
            if matches:
                return matches[-1]
    return ""


def parse_list_output(output):
    match = re.search(r"There are (\d+) of a max of (\d+) players online:?\s*(.*)", output or "")
    if not match:
        return 0, 0, []
    players = [p.strip() for p in match.group(3).split(",") if p.strip()] if match.group(3).strip() else []
    return int(match.group(1)), int(match.group(2)), players


def parse_tick_output(output):
    match = re.search(r"(?i)([0-9.]+)\s*ticks per second.*?([0-9.]+)\s*ms", output or "")
    if match:
        return match.group(1), match.group(2)
    match = re.search(r"(?is)Target tick rate:\s*([0-9.]+)\s*per second.*?Average time per tick:\s*([0-9.]+)\s*ms", output or "")
    if match:
        return match.group(1), match.group(2)
    return "", (output or "").strip()


def parse_playit_address(path):
    cache = Path(path).parent / "playit.address"
    try:
        content = Path(path).read_text(errors="ignore")
    except OSError:
        content = cache.read_text(errors="ignore") if cache.exists() else ""
    hosts = re.findall(r"(?i)[a-z0-9][a-z0-9.-]*\.(?:joinmc\.link|playit\.gg)(?::\d+)?", content)
    address = preferred_playit_host(hosts)
    if address:
        with contextlib.suppress(OSError):
            cache.write_text(address + "\n")
    return address


def preferred_playit_host(hosts):
    fallback = ""
    joinmc = ""
    for host in hosts:
        host = host.strip(".")
        if not host:
            continue
        fallback = host
        if ".joinmc.link" in host.lower():
            joinmc = host
    return joinmc or fallback


def safe_zip_name(world_name):
    name = Path(world_name).name.strip() or "world"
    return name + ".zip"


def zip_world(zf, world_path, world_name):
    root_name = Path(world_name).name.strip() or "world"
    for root, dirs, files in os.walk(world_path):
        dirs[:] = [d for d in dirs if not (Path(root) / d).is_symlink()]
        root_path = Path(root)
        rel_root = root_path.relative_to(world_path)
        for name in files:
            file_path = root_path / name
            if file_path.is_symlink():
                continue
            rel = file_path.relative_to(world_path)
            zf.write(file_path, Path(root_name) / rel)


def clean_zip_path(name):
    cleaned = Path(name.replace("\\", "/"))
    if cleaned.is_absolute() or ".." in cleaned.parts or str(cleaned) in ("", "."):
        raise RuntimeError(f"zip file contains unsafe path: {name}")
    return cleaned


def extract_world_zip(zip_path, dest):
    dest = Path(dest).resolve()
    dest.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(zip_path) as zf:
        if not zf.infolist():
            raise RuntimeError("zip file is empty")
        for info in zf.infolist():
            rel = clean_zip_path(info.filename)
            target = (dest / rel).resolve()
            if not str(target).startswith(str(dest)):
                raise RuntimeError(f"zip file contains unsafe path: {info.filename}")
            mode = info.external_attr >> 16
            if mode & 0o120000:
                raise RuntimeError(f"zip file contains unsupported symlink: {info.filename}")
            if info.is_dir():
                target.mkdir(parents=True, exist_ok=True)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            with zf.open(info) as src, target.open("wb") as out:
                shutil.copyfileobj(src, out)
    source = uploaded_world_root(dest)
    if not (source / "level.dat").exists():
        raise RuntimeError("zip does not contain a Minecraft world level.dat")
    return source


def uploaded_world_root(extract_dir):
    extract_dir = Path(extract_dir)
    if (extract_dir / "level.dat").exists():
        return extract_dir
    dirs = []
    for entry in extract_dir.iterdir():
        if entry.name in ("__MACOSX", ".DS_Store"):
            continue
        if entry.is_dir():
            dirs.append(entry)
        else:
            raise RuntimeError("zip must contain a single world folder or world files directly")
    if len(dirs) != 1:
        raise RuntimeError("zip must contain a single world folder or world files directly")
    return dirs[0]


def replace_world_dir(source, target):
    source = Path(source).resolve()
    target = Path(target).resolve()
    parent = target.parent.resolve()
    if not str(target).startswith(str(parent)):
        raise RuntimeError("unsafe world target")
    backup = target.with_name(target.name + ".backup-" + datetime.now().strftime("%Y%m%d-%H%M%S"))
    had_existing = target.exists()
    if had_existing:
        target.rename(backup)
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        source.rename(target)
        if had_existing:
            shutil.rmtree(backup, ignore_errors=True)
    except Exception:
        if had_existing and backup.exists() and not target.exists():
            backup.rename(target)
        raise


def split_host_port(addr):
    if addr.startswith("["):
        host, _, tail = addr[1:].partition("]")
        return host, tail.lstrip(":") or "8080"
    if ":" not in addr:
        return addr, "8080"
    host, port = addr.rsplit(":", 1)
    return host or "0.0.0.0", port


def start_daemon(cfg):
    cfg.runtime_dir.mkdir(parents=True, exist_ok=True)
    pid = read_pid(cfg.runtime_dir / PID_FILE)
    if process_running(pid):
        print("Minecraft monitor is already running.")
        return
    log_file = cfg.runtime_dir / "supervisor.log"
    out = log_file.open("ab")
    cmd = [sys.executable, str(Path(sys.argv[0]).resolve()), "-daemon"]
    proc = subprocess.Popen(cmd, cwd=str(cfg.root), stdout=out, stderr=out, stdin=subprocess.DEVNULL, start_new_session=True, env=os.environ.copy())
    print(f"Started Minecraft monitor with PID {proc.pid}.")
    print(f"Dashboard: http://{cfg.web_addr}")
    print(f"Logs: {log_file}")


def run_daemon(cfg):
    cfg.runtime_dir.mkdir(parents=True, exist_ok=True)
    cfg.monitor_dir.mkdir(parents=True, exist_ok=True)
    pid_path = cfg.runtime_dir / PID_FILE
    if process_running(read_pid(pid_path)):
        raise RuntimeError("another monitor is already running")
    write_pid(pid_path, os.getpid())
    log_file = (cfg.runtime_dir / "supervisor.log").open("a", buffering=1)

    def log(message):
        print(f"{now_iso()} {message}", file=log_file, flush=True)

    app = App(cfg, log)

    def handle_signal(signum, _frame):
        if signum == signal.SIGUSR2:
            app.handoff_event.set()
        app.stop_event.set()

    signal.signal(signal.SIGTERM, handle_signal)
    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGUSR2, handle_signal)
    try:
        app.run()
    finally:
        with contextlib.suppress(OSError):
            pid_path.unlink()
        log_file.close()


def stop_daemon(cfg):
    pid = read_pid(cfg.runtime_dir / PID_FILE)
    if not pid:
        raise RuntimeError("monitor is not running")
    os.kill(pid, signal.SIGTERM)
    print(f"Sent stop signal to monitor PID {pid}.")


def restart_monitor(cfg):
    pid = read_pid(cfg.runtime_dir / PID_FILE)
    if not process_running(pid):
        with contextlib.suppress(OSError):
            (cfg.runtime_dir / PID_FILE).unlink()
        print("Monitor is not running; starting it.")
        start_daemon(cfg)
        return
    os.kill(pid, signal.SIGUSR2)
    print(f"Sent handoff restart signal to monitor PID {pid}.")
    if not wait_for_exit(pid, 8):
        print(f"Monitor PID {pid} did not exit after handoff signal; killing only the monitor process.")
        os.kill(pid, signal.SIGKILL)
        if not wait_for_exit(pid, 8):
            raise RuntimeError(f"monitor PID {pid} did not exit after SIGKILL")
    with contextlib.suppress(OSError):
        (cfg.runtime_dir / PID_FILE).unlink()
    start_daemon(cfg)


def restart_all(cfg):
    pid = read_pid(cfg.runtime_dir / PID_FILE)
    if process_running(pid):
        os.kill(pid, signal.SIGTERM)
        print(f"Sent restart signal to monitor PID {pid}.")
        if not wait_for_exit(pid, 60):
            raise RuntimeError(f"monitor PID {pid} did not exit after restart signal")
    with contextlib.suppress(OSError):
        (cfg.runtime_dir / PID_FILE).unlink()
    start_daemon(cfg)


def monitor_action_url(cfg, target, action):
    host, port = split_host_port(cfg.web_addr)
    if host in ("", "0.0.0.0", "::", "[::]"):
        host = "127.0.0.1"
    return f"http://{host}:{port}/api/{target}/{action}"


def restart_managed_service(cfg, target):
    url = monitor_action_url(cfg, target, "restart")
    req = urllib.request.Request(url, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            if resp.status < 200 or resp.status >= 300:
                raise RuntimeError(f"{resp.status}: {resp.read(4096).decode(errors='ignore')}")
    except urllib.error.URLError as exc:
        raise RuntimeError(f"restart {target} through monitor failed: {exc}") from exc
    print(f"Requested {target} restart through monitor.")


def status(cfg):
    pid = read_pid(cfg.runtime_dir / PID_FILE)
    if process_running(pid):
        print(f"Monitor is running with PID {pid}.")
        print(f"Dashboard: http://{cfg.web_addr}")
        print(f"Logs: {cfg.runtime_dir / 'supervisor.log'}")
    elif pid:
        with contextlib.suppress(OSError):
            (cfg.runtime_dir / PID_FILE).unlink()
        print("Monitor PID file exists, but the process is not running.")
    else:
        print("Monitor is not running.")


def resolve_restart_target(target):
    target = (target or "all").lower()
    if target in ("", "all"):
        return "all"
    if target in ("mon", "monitor", "web"):
        return "monitor"
    if target in ("minecraft", "mc", "server"):
        return "minecraft"
    if target in ("playit", "conn", "connection"):
        return "playit"
    raise RuntimeError(f"unknown restart target {target!r}")


def main():
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("-mode", default="")
    parser.add_argument("-start", action="store_true")
    parser.add_argument("-daemon", action="store_true")
    parser.add_argument("-stop", action="store_true")
    parser.add_argument("-status", action="store_true")
    parser.add_argument("-configure", action="store_true")
    parser.add_argument("-restart", nargs="?", const="all")
    args, rest = parser.parse_known_args()
    selected = [name for name, enabled in {
        "start": args.start,
        "daemon": args.daemon,
        "stop": args.stop,
        "status": args.status,
        "configure": args.configure,
        "restart": args.restart is not None,
    }.items() if enabled]
    if args.mode:
        selected.append(args.mode)
    if len(selected) > 1:
        raise RuntimeError("choose only one command flag")
    action = selected[0] if selected else (rest[0] if rest else "start")
    target = args.restart if args.restart is not None else (rest[1] if len(rest) > 1 else "")
    cfg = load_config(create_secret=action in ("daemon", "configure"))

    if action == "start":
        start_daemon(cfg)
    elif action == "daemon":
        run_daemon(cfg)
    elif action == "stop":
        stop_daemon(cfg)
    elif action == "status":
        status(cfg)
    elif action == "configure":
        ensure_server_properties(cfg)
    elif action == "restart":
        restart_target = resolve_restart_target(target or (rest[0] if rest else "all"))
        if restart_target == "all":
            restart_all(cfg)
        elif restart_target == "monitor":
            restart_monitor(cfg)
        else:
            restart_managed_service(cfg, restart_target)
    elif action in ("restart-monitor",):
        restart_monitor(cfg)
    elif action in ("restart-all",):
        restart_all(cfg)
    elif action in ("restart-minecraft",):
        restart_managed_service(cfg, "minecraft")
    elif action in ("restart-playit",):
        restart_managed_service(cfg, "playit")
    else:
        raise RuntimeError(f"unknown action {action!r}")


DASHBOARD_HTML = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Minecraft Monitor</title>
<style>
body{font-family:system-ui,-apple-system,Segoe UI,sans-serif;margin:0;background:#101318;color:#eef2f7}
header,main{max-width:1180px;margin:0 auto;padding:20px}
h1{margin:0 0 4px;font-size:28px}.sub{color:#99a3b3;font-size:14px}
.grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px}.card{background:#171c24;border:1px solid #263040;border-radius:8px;padding:14px}
.wide{grid-column:span 2}.full{grid-column:1/-1}.metric{font-size:26px;font-weight:700}.ok{color:#68d391}.bad{color:#fc8181}
button,input,select{background:#232b38;color:#eef2f7;border:1px solid #3a4658;border-radius:6px;padding:8px}button{cursor:pointer}button:hover{background:#2d3748}
pre{white-space:pre-wrap;word-break:break-word;background:#0b0e13;border-radius:6px;padding:12px;max-height:420px;overflow:auto}
.row{display:flex;gap:8px;flex-wrap:wrap;align-items:center}.command{display:flex;gap:8px}.command input{flex:1}
@media(max-width:800px){.grid{grid-template-columns:1fr}.wide{grid-column:auto}}
</style>
</head>
<body>
<header><h1>Minecraft Monitor</h1><div class="sub" id="generated">Loading...</div></header>
<main class="grid">
<section class="card"><div class="sub">Minecraft</div><div class="metric" id="mcState">-</div><div id="mcInfo" class="sub"></div><div class="row"><button onclick="act('minecraft','start')">Start</button><button onclick="act('minecraft','stop')">Stop</button><button onclick="act('minecraft','restart')">Restart</button><button onclick="act('minecraft','kill')">Kill</button></div></section>
<section class="card"><div class="sub">playit</div><div class="metric" id="playitState">-</div><div id="playitAddress" class="sub"></div><div class="row"><button onclick="act('playit','start')">Start</button><button onclick="act('playit','stop')">Stop</button><button onclick="act('playit','restart')">Restart</button><button onclick="act('playit','kill')">Kill</button></div></section>
<section class="card"><div class="sub">Players</div><div class="metric" id="players">-</div><div id="playerList" class="sub"></div></section>
<section class="card"><div class="sub">World</div><div class="metric" id="worldName">-</div><div id="worldDetails" class="sub"></div><div class="row"><button onclick="downloadWorld()">Download ZIP</button></div></section>
<section class="card wide"><div class="sub">Machine</div><div id="machine"></div></section>
<section class="card wide"><div class="sub">Timing</div><div id="timing"></div></section>
<section class="card full"><div class="row"><select id="logTarget"><option value="supervisor">Supervisor</option><option value="minecraft">Minecraft</option><option value="playit">playit</option><option value="latest">Latest server log</option></select><input id="logSearch" placeholder="Search logs"><button onclick="loadLogs()">Refresh logs</button></div><pre id="logs">Loading...</pre></section>
<section class="card full"><form class="command" onsubmit="sendCommand(event)"><input id="commandInput" autocomplete="off" spellcheck="false" placeholder="/say hello"><button id="commandSend">Send</button></form><pre id="commandOutput"></pre></section>
</main>
<script>
const $=id=>document.getElementById(id);
async function json(url,opts){const r=await fetch(url,opts);if(!r.ok)throw new Error(await r.text());return r.json();}
function stateText(p){return p.running?'Running':(p.desired?'Starting':'Stopped')}
async function refresh(){
 const s=await json('/api/status');
 $('generated').textContent='Updated '+s.generatedAt+' | '+s.webAddr;
 $('mcState').textContent=stateText(s.processes.minecraft); $('mcState').className='metric '+(s.processes.minecraft.running?'ok':'bad');
 $('mcInfo').textContent=[s.minecraft.version&&('Version '+s.minecraft.version),s.minecraft.rconOk?'RCON OK':s.minecraft.lastError].filter(Boolean).join(' | ');
 $('playitState').textContent=stateText(s.processes.playit); $('playitState').className='metric '+(s.processes.playit.running?'ok':'bad');
 $('playitAddress').textContent=s.playit.address?('Server: '+s.playit.address):'Server address not detected yet';
 $('players').textContent=(s.minecraft.playersOnline||0)+' / '+(s.minecraft.maxPlayers||0);
 $('playerList').textContent=(s.minecraft.players||[]).join(', ');
 const w=s.minecraft.world||{}; $('worldName').textContent=w.name||'-'; $('worldDetails').textContent=[w.size&&('Size '+w.size),w.gameMode&&('Mode '+w.gameMode),w.difficulty&&('Difficulty '+w.difficulty),w.hardcore?'Hardcore':'Hardcore off'].filter(Boolean).join(' | ');
 const m=s.machine||{}; $('machine').textContent=`CPU ${m.cpuPercent||0}% | Memory ${m.memUsedMb||0}/${m.memTotalMb||0} MB (${m.memPercent||0}%) | Disk ${m.diskUsedMb||0}/${m.diskTotalMb||0} MB (${m.diskPercent||0}%)`;
 $('timing').textContent=[s.minecraft.tps&&('TPS '+s.minecraft.tps),s.minecraft.mspt&&('MSPT '+s.minecraft.mspt)].filter(Boolean).join(' | ')||'-';
}
async function loadLogs(){try{const d=await json('/api/logs?target='+encodeURIComponent($('logTarget').value)+'&q='+encodeURIComponent($('logSearch').value));$('logs').textContent=(d.lines||[]).join('\\n')||'No logs';}catch(e){$('logs').textContent=e.message}}
async function act(t,a){await json('/api/'+t+'/'+a,{method:'POST'});setTimeout(refresh,500)}
function downloadWorld(){location.href='/api/world/download'}
async function sendCommand(e){e.preventDefault();const c=$('commandInput').value.trim();if(!c)return;$('commandOutput').textContent='> '+c;try{const d=await json('/api/command',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({command:c})});$('commandOutput').textContent='> '+d.command+'\\n'+(d.output||'');$('commandInput').value='';}catch(err){$('commandOutput').textContent='Error: '+err.message}}
setInterval(refresh,5000);setInterval(loadLogs,5000);refresh();loadLogs();
</script>
</body>
</html>"""


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(exc, file=sys.stderr)
        sys.exit(1)
