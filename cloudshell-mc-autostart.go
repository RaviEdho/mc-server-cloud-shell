package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	lockFileName    = "supervisor.lock"
	pidFileName     = "supervisor.pid"
	metricRetention = 7 * 24 * time.Hour
)

type config struct {
	root           string
	runtimeDir     string
	monitorDir     string
	serverDir      string
	javaBin        string
	minRAM         string
	maxRAM         string
	socketPath     string
	secretPath     string
	webAddr        string
	rconHost       string
	rconPort       string
	rconPass       string
	healthInterval time.Duration
}

type app struct {
	cfg            config
	log            func(string, ...any)
	minecraft      *processManager
	playit         *processManager
	metrics        *metricStore
	rcon           *rconClient
	healthMu       sync.RWMutex
	minecraftCache minecraftStatus
	cpuLast        cpuSample
	cpuMu          sync.Mutex
}

type processManager struct {
	name        string
	dir         string
	args        []string
	adoptTokens []string
	beforeStart func()

	mu        sync.Mutex
	desired   bool
	running   bool
	pid       int
	startedAt time.Time
	stoppedAt time.Time
	lastError string
	cmd       *exec.Cmd
	adopted   bool
}

type processSnapshot struct {
	Desired   bool   `json:"desired"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt,omitempty"`
	StoppedAt string `json:"stoppedAt,omitempty"`
	LastError string `json:"lastError,omitempty"`
	UptimeSec int64  `json:"uptimeSec"`
}

type metricPoint struct {
	Time        string  `json:"time"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemUsedMB   uint64  `json:"memUsedMb"`
	MemTotalMB  uint64  `json:"memTotalMb"`
	MemPercent  float64 `json:"memPercent"`
	DiskUsedMB  uint64  `json:"diskUsedMb"`
	DiskTotalMB uint64  `json:"diskTotalMb"`
	DiskPercent float64 `json:"diskPercent"`
}

type metricStore struct {
	mu     sync.Mutex
	points []metricPoint
	path   string
}

type cpuSample struct {
	total uint64
	idle  uint64
	ok    bool
}

type statusResponse struct {
	GeneratedAt string                     `json:"generatedAt"`
	WebAddr     string                     `json:"webAddr"`
	PreviewHint string                     `json:"previewHint"`
	Machine     metricPoint                `json:"machine"`
	Minecraft   minecraftStatus            `json:"minecraft"`
	Playit      playitStatus               `json:"playit"`
	Processes   map[string]processSnapshot `json:"processes"`
}

type minecraftStatus struct {
	UpdatedAt     string   `json:"updatedAt,omitempty"`
	PortOpen      bool     `json:"portOpen"`
	RCONOK        bool     `json:"rconOk"`
	Version       string   `json:"version"`
	PlayersOnline int      `json:"playersOnline"`
	MaxPlayers    int      `json:"maxPlayers"`
	Players       []string `json:"players"`
	TPS           string   `json:"tps"`
	MSPT          string   `json:"mspt"`
	LastError     string   `json:"lastError,omitempty"`
}

type playitStatus struct {
	SocketExists bool   `json:"socketExists"`
	Address      string `json:"address"`
	LastError    string `json:"lastError,omitempty"`
}

func main() {
	mode := flag.String("mode", "start", "start, daemon, stop, restart-monitor, status, or configure")
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		fatal(err)
	}

	switch *mode {
	case "start":
		err = startDaemon(cfg)
	case "daemon":
		err = runDaemon(cfg)
	case "stop":
		err = stopDaemon(cfg)
	case "restart-monitor":
		err = restartMonitor(cfg)
	case "status":
		err = status(cfg)
	case "configure":
		err = ensureServerProperties(cfg)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fatal(err)
	}
}

func loadConfig() (config, error) {
	exe, err := os.Executable()
	if err != nil {
		return config{}, err
	}
	root := os.Getenv("MC_AUTOSTART_ROOT")
	if root == "" {
		root = filepath.Dir(exe)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return config{}, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return config{}, err
	}

	javaBin := os.Getenv("JAVA_BIN")
	if javaBin == "" {
		sdkmanJava := filepath.Join(home, ".sdkman", "candidates", "java", "current", "bin", "java")
		if fileExists(sdkmanJava) {
			javaBin = sdkmanJava
		} else {
			javaBin = "java"
		}
	}

	serverDir := os.Getenv("MC_SERVER_DIR")
	if serverDir == "" {
		if fileExists(filepath.Join(root, "fabric-server-launch.jar")) {
			serverDir = root
		} else {
			serverDir = filepath.Join(root, "server")
		}
	}
	serverDir, err = filepath.Abs(serverDir)
	if err != nil {
		return config{}, err
	}

	runtimeDir := filepath.Join(root, ".runtime")
	rconPass := os.Getenv("MC_RCON_PASSWORD")
	if rconPass == "" {
		rconPass = readOrCreateSecret(filepath.Join(runtimeDir, "rcon.password"))
	}

	return config{
		root:           root,
		runtimeDir:     runtimeDir,
		monitorDir:     filepath.Join(root, ".monitor"),
		serverDir:      serverDir,
		javaBin:        javaBin,
		minRAM:         envDefault("MC_MIN_RAM", "1G"),
		maxRAM:         envDefault("MC_MAX_RAM", "2G"),
		socketPath:     envDefault("PLAYIT_SOCKET", "/tmp/playit.sock"),
		secretPath:     envDefault("PLAYIT_SECRET", filepath.Join(home, ".config", "playit_gg", "playit.toml")),
		webAddr:        envDefault("MC_MONITOR_ADDR", "0.0.0.0:8080"),
		rconHost:       envDefault("MC_RCON_HOST", "127.0.0.1"),
		rconPort:       envDefault("MC_RCON_PORT", "25575"),
		rconPass:       rconPass,
		healthInterval: durationEnvDefault("MC_HEALTH_INTERVAL", 30*time.Second),
	}, nil
}

func startDaemon(cfg config) error {
	if err := os.MkdirAll(cfg.runtimeDir, 0755); err != nil {
		return err
	}
	lock, locked, err := tryLock(cfg)
	if err != nil {
		return err
	}
	if !locked {
		fmt.Println("Minecraft monitor is already running.")
		return nil
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(cfg.runtimeDir, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "-mode", "daemon")
	cmd.Dir = cfg.root
	cmd.Env = os.Environ()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Printf("Started Minecraft monitor with PID %d.\n", cmd.Process.Pid)
	fmt.Printf("Dashboard: http://%s\n", cfg.webAddr)
	fmt.Printf("Logs: %s\n", filepath.Join(cfg.runtimeDir, "supervisor.log"))
	return nil
}

func runDaemon(cfg config) error {
	if err := os.MkdirAll(cfg.runtimeDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.monitorDir, 0755); err != nil {
		return err
	}
	if err := ensureServerProperties(cfg); err != nil {
		return err
	}

	lock, locked, err := tryLock(cfg)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("another monitor is already running")
	}
	defer lock.Close()

	if err := os.WriteFile(filepath.Join(cfg.runtimeDir, pidFileName), []byte(strconv.Itoa(os.Getpid())+"\n"), 0644); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(cfg.runtimeDir, pidFileName))

	logf, err := os.OpenFile(filepath.Join(cfg.runtimeDir, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer logf.Close()
	log := func(format string, args ...any) {
		fmt.Fprintf(logf, "%s ", time.Now().Format(time.RFC3339))
		fmt.Fprintf(logf, format+"\n", args...)
	}

	if err := validateConfig(cfg); err != nil {
		log("configuration error: %v", err)
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR2)
	defer signal.Stop(sigCh)
	var handoff atomic.Bool
	go func() {
		sig := <-sigCh
		if sig == syscall.SIGUSR2 {
			handoff.Store(true)
		}
		cancel()
	}()

	a := &app{
		cfg:     cfg,
		log:     log,
		metrics: newMetricStore(filepath.Join(cfg.monitorDir, "metrics.jsonl")),
		rcon:    newRCONClient(net.JoinHostPort(cfg.rconHost, cfg.rconPort), cfg.rconPass),
	}
	a.playit = newProcessManager("playit", cfg.root, []string{
		filepath.Join(cfg.root, "playit-linux-amd64"),
		"--socket-path", cfg.socketPath,
		"--secret-path", cfg.secretPath,
	}, []string{"playit-linux-amd64", "--socket-path", cfg.socketPath}, func() {
		_ = os.Remove(cfg.socketPath)
	})
	a.minecraft = newProcessManager("minecraft", cfg.serverDir, []string{
		cfg.javaBin,
		"-Xms" + cfg.minRAM,
		"-Xmx" + cfg.maxRAM,
		"-jar", "fabric-server-launch.jar",
		"nogui",
	}, []string{"fabric-server-launch.jar", "nogui"}, nil)

	log("monitor started root=%s web=%s java=%s", cfg.root, cfg.webAddr, cfg.javaBin)

	var wg sync.WaitGroup
	a.playit.startDesired()
	a.minecraft.startDesired()
	for _, pm := range []*processManager{a.playit, a.minecraft} {
		wg.Add(1)
		go func(pm *processManager) {
			defer wg.Done()
			pm.supervise(ctx, cfg.runtimeDir, &handoff, log)
		}(pm)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.collectMetrics(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.collectMinecraftHealth(ctx)
	}()

	srv := &http.Server{
		Addr:              cfg.webAddr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log("dashboard listening on http://%s", cfg.webAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log("dashboard error: %v", err)
		}
	}()

	<-ctx.Done()
	if handoff.Load() {
		log("monitor handoff requested")
	} else {
		log("monitor stopping")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	a.rcon.close()
	if !handoff.Load() {
		a.minecraft.stopGracefully(30*time.Second, log)
		a.playit.stopGracefully(10*time.Second, log)
	}
	wg.Wait()
	if !handoff.Load() {
		_ = os.Remove(cfg.socketPath)
	}
	log("monitor stopped")
	return nil
}

func newProcessManager(name, dir string, args []string, adoptTokens []string, beforeStart func()) *processManager {
	return &processManager{name: name, dir: dir, args: args, adoptTokens: adoptTokens, beforeStart: beforeStart}
}

func (pm *processManager) startDesired() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.desired = true
}

func (pm *processManager) stopDesired() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.desired = false
}

func (pm *processManager) snapshot() processSnapshot {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	s := processSnapshot{
		Desired:   pm.desired,
		Running:   pm.running,
		PID:       pm.pid,
		LastError: pm.lastError,
	}
	if !pm.startedAt.IsZero() {
		s.StartedAt = pm.startedAt.Format(time.RFC3339)
		if pm.running {
			s.UptimeSec = int64(time.Since(pm.startedAt).Seconds())
		}
	}
	if !pm.stoppedAt.IsZero() {
		s.StoppedAt = pm.stoppedAt.Format(time.RFC3339)
	}
	return s
}

func (pm *processManager) supervise(ctx context.Context, runtimeDir string, handoff *atomic.Bool, log func(string, ...any)) {
	backoff := 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !pm.isDesired() {
			time.Sleep(time.Second)
			continue
		}
		if pid := pm.findAdoptablePID(runtimeDir); pid > 0 {
			pm.markAdopted(pid)
			log("adopted existing %s process group %d", pm.name, pid)
			pm.monitorAdopted(ctx, runtimeDir, log)
			continue
		}
		if pm.beforeStart != nil {
			pm.beforeStart()
		}
		out, err := os.OpenFile(filepath.Join(runtimeDir, pm.name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			pm.setError(err.Error())
			log("%s log open failed: %v", pm.name, err)
			sleepOrDone(ctx, backoff)
			continue
		}
		cmd := exec.Command(pm.args[0], pm.args[1:]...)
		cmd.Dir = pm.dir
		cmd.Stdout = out
		cmd.Stderr = out
		cmd.Stdin = nil
		cmd.Env = os.Environ()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		log("starting %s: %s", pm.name, strings.Join(pm.args, " "))
		if err := cmd.Start(); err != nil {
			_ = out.Close()
			pm.setError(err.Error())
			log("%s failed to start: %v", pm.name, err)
			sleepOrDone(ctx, backoff)
			continue
		}
		pm.writePID(runtimeDir, cmd.Process.Pid, log)
		pm.markStarted(cmd)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err = <-done:
			_ = out.Close()
			pm.markStopped(err)
			pm.removePID(runtimeDir)
		case <-ctx.Done():
			if handoff.Load() {
				_ = out.Close()
				return
			}
			log("stopping %s", pm.name)
			stopProcessGroup(cmd.Process.Pid, 10*time.Second, log)
			err = <-done
			_ = out.Close()
			pm.markStopped(err)
			pm.removePID(runtimeDir)
			return
		}
		if err != nil {
			log("%s exited: %v", pm.name, err)
		} else {
			log("%s exited normally", pm.name)
		}
		if !pm.isDesired() {
			continue
		}
		log("restarting %s in %s", pm.name, backoff)
		if !sleepOrDone(ctx, backoff) {
			return
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (pm *processManager) isDesired() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.desired
}

func (pm *processManager) monitorAdopted(ctx context.Context, runtimeDir string, log func(string, ...any)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
			pm.mu.Lock()
			pid := pm.pid
			desired := pm.desired
			pm.mu.Unlock()
			if pid <= 0 {
				return
			}
			if !desired {
				log("stopping adopted %s process group %d", pm.name, pid)
				stopProcessGroup(pid, 10*time.Second, log)
			}
			if !desired || !processRunning(pid) {
				pm.markStopped(nil)
				pm.removePID(runtimeDir)
				return
			}
		}
	}
}

func (pm *processManager) markStarted(cmd *exec.Cmd) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.cmd = cmd
	pm.pid = cmd.Process.Pid
	pm.running = true
	pm.adopted = false
	pm.startedAt = time.Now()
	pm.stoppedAt = time.Time{}
	pm.lastError = ""
}

func (pm *processManager) markAdopted(pid int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.cmd = nil
	pm.pid = pid
	pm.running = true
	pm.adopted = true
	if pm.startedAt.IsZero() {
		pm.startedAt = time.Now()
	}
	pm.stoppedAt = time.Time{}
	pm.lastError = ""
}

func (pm *processManager) markStopped(err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.cmd = nil
	pm.pid = 0
	pm.running = false
	pm.adopted = false
	pm.stoppedAt = time.Now()
	if err != nil {
		pm.lastError = err.Error()
	}
}

func (pm *processManager) setError(msg string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.lastError = msg
}

func (pm *processManager) pidPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, pm.name+".pid")
}

func (pm *processManager) writePID(runtimeDir string, pid int, log func(string, ...any)) {
	if err := os.WriteFile(pm.pidPath(runtimeDir), []byte(strconv.Itoa(pid)+"\n"), 0644); err != nil {
		log("write %s pid failed: %v", pm.name, err)
	}
}

func (pm *processManager) removePID(runtimeDir string) {
	_ = os.Remove(pm.pidPath(runtimeDir))
}

func (pm *processManager) findAdoptablePID(runtimeDir string) int {
	if pid := readProcessPID(pm.pidPath(runtimeDir)); processRunning(pid) {
		return pid
	}
	pid := findProcessByCmdline(pm.adoptTokens)
	if pid > 0 {
		return pid
	}
	pm.removePID(runtimeDir)
	return 0
}

func (pm *processManager) stopGracefully(timeout time.Duration, log func(string, ...any)) {
	pm.stopDesired()
	pm.mu.Lock()
	cmd := pm.cmd
	pid := pm.pid
	pm.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	if pid <= 0 {
		return
	}
	stopProcessGroup(pid, timeout, log)
}

func (pm *processManager) kill(log func(string, ...any)) {
	pm.stopDesired()
	pm.mu.Lock()
	cmd := pm.cmd
	pid := pm.pid
	pm.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	log("killed %s process group %d", pm.name, pid)
}

func stopProcessGroup(pid int, timeout time.Duration, log func(string, ...any)) {
	if pid <= 0 {
		return
	}
	pgid := -pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		log("SIGTERM failed for process group %d: %v", pid, err)
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pgid, syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/metrics", a.handleMetrics)
	mux.HandleFunc("/api/logs", a.handleLogs)
	mux.HandleFunc("/api/minecraft/", a.handleAction("minecraft"))
	mux.HandleFunc("/api/playit/", a.handleAction("playit"))
	return mux
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboardTemplate.Execute(w, nil)
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.status())
}

func (a *app) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.metrics.since(metricRetention))
}

func (a *app) handleLogs(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	var path string
	switch target {
	case "minecraft":
		path = filepath.Join(a.cfg.runtimeDir, "minecraft.log")
	case "playit":
		path = filepath.Join(a.cfg.runtimeDir, "playit.log")
	case "server":
		path = filepath.Join(a.cfg.serverDir, "logs", "latest.log")
	default:
		path = filepath.Join(a.cfg.runtimeDir, "supervisor.log")
	}
	lines, err := tailLines(path, 200, search)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"target": target, "lines": lines})
}

func (a *app) handleAction(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		action := strings.TrimPrefix(r.URL.Path, "/api/"+target+"/")
		pm := a.playit
		if target == "minecraft" {
			pm = a.minecraft
		}
		switch action {
		case "start":
			pm.startDesired()
		case "stop":
			if target == "minecraft" {
				pm.stopDesired()
				if _, err := a.rconCommand("stop"); err != nil {
					a.log("rcon stop returned: %v", err)
				}
			} else {
				pm.stopGracefully(10*time.Second, a.log)
			}
		case "restart":
			if target == "minecraft" {
				pm.stopDesired()
				_, _ = a.rconCommand("stop")
			}
			pm.stopGracefully(20*time.Second, a.log)
			pm.startDesired()
		case "kill":
			pm.kill(a.log)
		default:
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]string{"ok": "true"})
	}
}

func (a *app) status() statusResponse {
	machine := a.collectMachineMetric()
	mc := a.cachedMinecraftHealth()
	playit := a.playitHealth()
	preview := ""
	if webHost := os.Getenv("WEB_HOST"); webHost != "" {
		_, port, _ := net.SplitHostPort(a.cfg.webAddr)
		preview = "https://" + port + "-" + webHost
	}
	return statusResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		WebAddr:     a.cfg.webAddr,
		PreviewHint: preview,
		Machine:     machine,
		Minecraft:   mc,
		Playit:      playit,
		Processes: map[string]processSnapshot{
			"minecraft": a.minecraft.snapshot(),
			"playit":    a.playit.snapshot(),
		},
	}
}

func (a *app) cachedMinecraftHealth() minecraftStatus {
	a.healthMu.RLock()
	defer a.healthMu.RUnlock()
	if a.minecraftCache.UpdatedAt != "" {
		return a.minecraftCache
	}
	return minecraftStatus{
		UpdatedAt:  time.Now().Format(time.RFC3339),
		PortOpen:   dialOK("127.0.0.1:25565", 700*time.Millisecond),
		Version:    parseServerVersion(filepath.Join(a.cfg.runtimeDir, "minecraft.log"), filepath.Join(a.cfg.serverDir, "logs", "latest.log")),
		MaxPlayers: readMaxPlayers(filepath.Join(a.cfg.serverDir, "server.properties")),
		Players:    []string{},
		LastError:  "waiting for first health sample",
	}
}

func (a *app) refreshMinecraftHealth() {
	health := a.minecraftHealth()
	health.UpdatedAt = time.Now().Format(time.RFC3339)
	if health.Players == nil {
		health.Players = []string{}
	}
	a.healthMu.Lock()
	a.minecraftCache = health
	a.healthMu.Unlock()
}

func (a *app) minecraftHealth() minecraftStatus {
	result := minecraftStatus{
		PortOpen: dialOK("127.0.0.1:25565", 700*time.Millisecond),
		Version:  parseServerVersion(filepath.Join(a.cfg.runtimeDir, "minecraft.log"), filepath.Join(a.cfg.serverDir, "logs", "latest.log")),
		Players:  []string{},
	}
	out, err := a.rconCommands("list", "tick query")
	if err != nil {
		result.LastError = err.Error()
		result.MaxPlayers = readMaxPlayers(filepath.Join(a.cfg.serverDir, "server.properties"))
		return result
	}
	result.RCONOK = true
	result.PlayersOnline, result.MaxPlayers, result.Players = parseListOutput(out[0])
	if result.MaxPlayers == 0 {
		result.MaxPlayers = readMaxPlayers(filepath.Join(a.cfg.serverDir, "server.properties"))
	}
	if len(out) > 1 {
		result.TPS, result.MSPT = parseTickOutput(out[1])
	}
	return result
}

func (a *app) playitHealth() playitStatus {
	return playitStatus{
		SocketExists: fileExists(a.cfg.socketPath),
		Address:      parsePlayitAddress(filepath.Join(a.cfg.runtimeDir, "playit.log")),
	}
}

func (a *app) collectMetrics(ctx context.Context) {
	a.metrics.add(a.collectMachineMetric())
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.metrics.add(a.collectMachineMetric())
			a.metrics.prune(metricRetention)
		}
	}
}

func (a *app) collectMinecraftHealth(ctx context.Context) {
	a.refreshMinecraftHealth()
	ticker := time.NewTicker(a.cfg.healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.refreshMinecraftHealth()
		}
	}
}

func (a *app) collectMachineMetric() metricPoint {
	cpu := a.cpuPercent()
	memUsed, memTotal := memUsage()
	diskUsed, diskTotal := diskUsage(a.cfg.root)
	return metricPoint{
		Time:        time.Now().Format(time.RFC3339),
		CPUPercent:  round(cpu),
		MemUsedMB:   memUsed / 1024 / 1024,
		MemTotalMB:  memTotal / 1024 / 1024,
		MemPercent:  percent(memUsed, memTotal),
		DiskUsedMB:  diskUsed / 1024 / 1024,
		DiskTotalMB: diskTotal / 1024 / 1024,
		DiskPercent: percent(diskUsed, diskTotal),
	}
}

func (a *app) cpuPercent() float64 {
	now, ok := readCPU()
	if !ok {
		return 0
	}
	a.cpuMu.Lock()
	defer a.cpuMu.Unlock()
	if !a.cpuLast.ok {
		a.cpuLast = now
		return 0
	}
	totalDelta := now.total - a.cpuLast.total
	idleDelta := now.idle - a.cpuLast.idle
	a.cpuLast = now
	if totalDelta == 0 {
		return 0
	}
	return float64(totalDelta-idleDelta) * 100 / float64(totalDelta)
}

func newMetricStore(path string) *metricStore {
	ms := &metricStore{path: path}
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	if file, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var p metricPoint
			if json.Unmarshal(scanner.Bytes(), &p) == nil {
				ms.points = append(ms.points, p)
			}
		}
		_ = file.Close()
	}
	return ms
}

func (ms *metricStore) add(p metricPoint) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.points = append(ms.points, p)
	file, err := os.OpenFile(ms.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer file.Close()
	data, _ := json.Marshal(p)
	_, _ = file.Write(append(data, '\n'))
}

func (ms *metricStore) since(maxAge time.Duration) []metricPoint {
	cutoff := time.Now().Add(-maxAge)
	ms.mu.Lock()
	defer ms.mu.Unlock()
	points := make([]metricPoint, 0, len(ms.points))
	for _, p := range ms.points {
		t, err := time.Parse(time.RFC3339, p.Time)
		if err == nil && t.After(cutoff) {
			points = append(points, p)
		}
	}
	return points
}

func (ms *metricStore) prune(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	ms.mu.Lock()
	defer ms.mu.Unlock()
	kept := ms.points[:0]
	for _, p := range ms.points {
		t, err := time.Parse(time.RFC3339, p.Time)
		if err == nil && t.After(cutoff) {
			kept = append(kept, p)
		}
	}
	ms.points = kept
	file, err := os.Create(ms.path)
	if err != nil {
		return
	}
	defer file.Close()
	for _, p := range ms.points {
		data, _ := json.Marshal(p)
		_, _ = file.Write(append(data, '\n'))
	}
}

func (a *app) rconCommand(command string) (string, error) {
	return a.rcon.command(command)
}

func (a *app) rconCommands(commands ...string) ([]string, error) {
	return a.rcon.commands(commands...)
}

type rconClient struct {
	mu       sync.Mutex
	addr     string
	password string
	conn     net.Conn
	nextID   int32
}

func newRCONClient(addr, password string) *rconClient {
	return &rconClient{
		addr:     addr,
		password: password,
		nextID:   10,
	}
}

func (c *rconClient) command(command string) (string, error) {
	out, err := c.commands(command)
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", nil
	}
	return out[0], nil
}

func (c *rconClient) commands(commands ...string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLocked(); err != nil {
		return nil, err
	}
	out, err := c.commandsLocked(commands...)
	if err != nil {
		c.closeLocked()
		if retryErr := c.ensureLocked(); retryErr != nil {
			return nil, err
		}
		out, err = c.commandsLocked(commands...)
	}
	return out, err
}

func (c *rconClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *rconClient) ensureLocked() error {
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("tcp", c.addr, 2*time.Second)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	id := c.nextLocked()
	if err := rconWrite(conn, id, 3, c.password); err != nil {
		_ = conn.Close()
		return err
	}
	respID, _, _, err := rconRead(conn)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if respID == -1 {
		_ = conn.Close()
		return errors.New("rcon authentication failed")
	}
	c.conn = conn
	return nil
}

func (c *rconClient) commandsLocked(commands ...string) ([]string, error) {
	out := make([]string, 0, len(commands))
	for _, command := range commands {
		_ = c.conn.SetDeadline(time.Now().Add(4 * time.Second))
		id := c.nextLocked()
		if err := rconWrite(c.conn, id, 2, command); err != nil {
			return out, err
		}
		respID, _, body, err := rconRead(c.conn)
		if err != nil {
			return out, err
		}
		if respID != id {
			return out, fmt.Errorf("unexpected rcon response id %d for request %d", respID, id)
		}
		out = append(out, body)
	}
	return out, nil
}

func (c *rconClient) nextLocked() int32 {
	c.nextID++
	return c.nextID
}

func (c *rconClient) closeLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func rconWrite(w io.Writer, id, typ int32, body string) error {
	payload := []byte(body)
	length := int32(4 + 4 + len(payload) + 2)
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, length)
	_ = binary.Write(buf, binary.LittleEndian, id)
	_ = binary.Write(buf, binary.LittleEndian, typ)
	buf.Write(payload)
	buf.Write([]byte{0, 0})
	_, err := w.Write(buf.Bytes())
	return err
}

func rconRead(r io.Reader) (int32, int32, string, error) {
	var length int32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return 0, 0, "", err
	}
	if length < 10 || length > 4096 {
		return 0, 0, "", fmt.Errorf("invalid rcon packet length %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, 0, "", err
	}
	id := int32(binary.LittleEndian.Uint32(buf[0:4]))
	typ := int32(binary.LittleEndian.Uint32(buf[4:8]))
	body := string(bytes.TrimRight(buf[8:], "\x00"))
	return id, typ, body, nil
}

func ensureServerProperties(cfg config) error {
	path := filepath.Join(cfg.serverDir, "server.properties")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	props := parseProperties(string(data))
	props["enable-rcon"] = "true"
	props["rcon.port"] = cfg.rconPort
	props["rcon.password"] = cfg.rconPass
	props["enable-query"] = "true"
	if props["query.port"] == "" {
		props["query.port"] = "25565"
	}
	return writeProperties(path, string(data), props)
}

func parseProperties(content string) map[string]string {
	props := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		props[parts[0]] = parts[1]
	}
	return props
}

func writeProperties(path, original string, updates map[string]string) error {
	seen := map[string]bool{}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(original))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "=") {
			lines = append(lines, line)
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := parts[0]
		if value, ok := updates[key]; ok {
			lines = append(lines, key+"="+value)
			seen[key] = true
		} else {
			lines = append(lines, line)
		}
	}
	var missing []string
	for key := range updates {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		lines = append(lines, key+"="+updates[key])
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

func readCPU() (cpuSample, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, false
	}
	var values []uint64
	for _, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		values = append(values, v)
	}
	var total uint64
	for _, v := range values {
		total += v
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return cpuSample{total: total, idle: idle, ok: true}, true
}

func memUsage() (used, total uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	vals := map[string]uint64{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) >= 2 {
			v, _ := strconv.ParseUint(parts[1], 10, 64)
			vals[strings.TrimSuffix(parts[0], ":")] = v * 1024
		}
	}
	total = vals["MemTotal"]
	available := vals["MemAvailable"]
	if total > available {
		used = total - available
	}
	return used, total
}

func diskUsage(path string) (used, total uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	total = stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	if total > free {
		used = total - free
	}
	return used, total
}

func parseListOutput(out string) (int, int, []string) {
	re := regexp.MustCompile(`There are (\d+) of a max of (\d+) players online:?\s*(.*)`)
	matches := re.FindStringSubmatch(out)
	if len(matches) == 0 {
		return 0, 0, []string{}
	}
	online, _ := strconv.Atoi(matches[1])
	maxPlayers, _ := strconv.Atoi(matches[2])
	var players []string
	if len(matches) > 3 && strings.TrimSpace(matches[3]) != "" {
		for _, player := range strings.Split(matches[3], ",") {
			players = append(players, strings.TrimSpace(player))
		}
	}
	return online, maxPlayers, players
}

func parseTickOutput(out string) (string, string) {
	re := regexp.MustCompile(`(?i)([0-9.]+)\s*ticks per second.*?([0-9.]+)\s*ms`)
	m := re.FindStringSubmatch(out)
	if len(m) == 3 {
		return m[1], m[2]
	}
	re = regexp.MustCompile(`(?is)Target tick rate:\s*([0-9.]+)\s*per second.*?Average time per tick:\s*([0-9.]+)\s*ms`)
	m = re.FindStringSubmatch(out)
	if len(m) == 3 {
		return m[1], m[2]
	}
	return "", strings.TrimSpace(out)
}

func parseServerVersion(paths ...string) string {
	re := regexp.MustCompile(`Starting minecraft server version ([^ \n]+)`)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		matches := re.FindAllStringSubmatch(string(data), -1)
		if len(matches) > 0 {
			return matches[len(matches)-1][1]
		}
	}
	return ""
}

func parsePlayitAddress(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`[a-zA-Z0-9.-]+\.playit\.gg(?::\d+)?`)
	matches := re.FindAllString(string(data), -1)
	if len(matches) > 0 {
		return matches[len(matches)-1]
	}
	re = regexp.MustCompile(`connect_addr:\s*([0-9.]+:\d+)`)
	submatches := re.FindAllStringSubmatch(string(data), -1)
	if len(submatches) > 0 {
		return submatches[len(submatches)-1][1]
	}
	re = regexp.MustCompile(`address:\s*([0-9.]+:\d+)`)
	submatches = re.FindAllStringSubmatch(string(data), -1)
	if len(submatches) > 0 {
		return submatches[len(submatches)-1][1]
	}
	return ""
}

func readMaxPlayers(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	props := parseProperties(string(data))
	v, _ := strconv.Atoi(props["max-players"])
	return v
}

func tailLines(path string, limit int, search string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	all := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var filtered []string
	for _, line := range all {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if search == "" || strings.Contains(strings.ToLower(line), strings.ToLower(search)) {
			filtered = append(filtered, line)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, nil
}

func dialOK(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func stopDaemon(cfg config) error {
	pid, err := readPID(cfg)
	if err != nil {
		return err
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	fmt.Printf("Sent stop signal to monitor PID %d.\n", pid)
	return nil
}

func restartMonitor(cfg config) error {
	pid, err := readPID(cfg)
	if err != nil {
		fmt.Println("Monitor is not running; starting it.")
		return startDaemon(cfg)
	}
	if !processRunning(pid) {
		_ = os.Remove(filepath.Join(cfg.runtimeDir, pidFileName))
		fmt.Println("Monitor PID file exists, but the process is not running; starting it.")
		return startDaemon(cfg)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGUSR2); err != nil {
		return err
	}
	fmt.Printf("Sent handoff restart signal to monitor PID %d.\n", pid)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(pid) {
			_ = os.Remove(filepath.Join(cfg.runtimeDir, pidFileName))
			return startDaemon(cfg)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("monitor PID %d did not exit after handoff signal", pid)
}

func status(cfg config) error {
	pid, err := readPID(cfg)
	if err != nil {
		fmt.Println("Monitor is not running.")
		return nil
	}
	if processRunning(pid) {
		fmt.Printf("Monitor is running with PID %d.\n", pid)
		fmt.Printf("Dashboard: http://%s\n", cfg.webAddr)
		if webHost := os.Getenv("WEB_HOST"); webHost != "" {
			_, port, _ := net.SplitHostPort(cfg.webAddr)
			fmt.Printf("Cloud Shell preview: https://%s-%s\n", port, webHost)
		}
		fmt.Printf("Logs: %s\n", filepath.Join(cfg.runtimeDir, "supervisor.log"))
		return nil
	}
	_ = os.Remove(filepath.Join(cfg.runtimeDir, pidFileName))
	fmt.Println("Monitor PID file exists, but the process is not running.")
	return nil
}

func validateConfig(cfg config) error {
	if !fileExists(filepath.Join(cfg.serverDir, "fabric-server-launch.jar")) {
		return fmt.Errorf("missing %s", filepath.Join(cfg.serverDir, "fabric-server-launch.jar"))
	}
	if !fileExists(filepath.Join(cfg.root, "playit-linux-amd64")) {
		return fmt.Errorf("missing %s", filepath.Join(cfg.root, "playit-linux-amd64"))
	}
	if !fileExists(cfg.secretPath) {
		return fmt.Errorf("missing playit secret at %s; run playit CLI setup first", cfg.secretPath)
	}
	if !fileExists(cfg.javaBin) && strings.Contains(cfg.javaBin, string(os.PathSeparator)) {
		return fmt.Errorf("missing Java binary at %s", cfg.javaBin)
	}
	return nil
}

func tryLock(cfg config) (*os.File, bool, error) {
	lock, err := os.OpenFile(filepath.Join(cfg.runtimeDir, lockFileName), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return lock, true, nil
}

func readPID(cfg config) (int, error) {
	data, err := os.ReadFile(filepath.Join(cfg.runtimeDir, pidFileName))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func processRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func readProcessPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

func findProcessByCmdline(tokens []string) int {
	if len(tokens) == 0 {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	self := os.Getpid()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		cmdline := strings.ReplaceAll(string(data), "\x00", " ")
		matched := true
		for _, token := range tokens {
			if token == "" {
				continue
			}
			if !strings.Contains(cmdline, token) {
				matched = false
				break
			}
		}
		if matched && processRunning(pid) {
			return pid
		}
	}
	return 0
}

func readOrCreateSecret(path string) string {
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data))
	}
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "change-me-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	secret := hex.EncodeToString(buf)
	_ = os.WriteFile(path, []byte(secret+"\n"), 0600)
	return secret
}

func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return round(float64(used) * 100 / float64(total))
}

func round(v float64) float64 {
	return math.Round(v*10) / 10
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func durationEnvDefault(name string, fallback time.Duration) time.Duration {
	if value := os.Getenv(name); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Minecraft Monitor</title>
<style>
:root {
  color-scheme: dark;
  --bg: #101214;
  --panel: #181b1f;
  --panel2: #20242a;
  --text: #e8ecef;
  --muted: #9ba7b3;
  --line: #333941;
  --good: #31c48d;
  --warn: #f6ad55;
  --bad: #f56565;
  --accent: #63b3ed;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font: 14px/1.4 system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 18px 22px;
  border-bottom: 1px solid var(--line);
}
h1 { margin: 0; font-size: 20px; font-weight: 650; }
main { padding: 18px 22px 28px; max-width: 1440px; margin: 0 auto; }
.grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; }
.wide { grid-column: span 2; }
.full { grid-column: 1 / -1; }
.panel {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 14px;
}
.panel h2 { margin: 0 0 12px; font-size: 14px; color: var(--muted); font-weight: 600; }
.metric { font-size: 26px; font-weight: 700; }
.sub { color: var(--muted); font-size: 12px; margin-top: 4px; }
.row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.row + .row { margin-top: 8px; }
.pill { padding: 3px 8px; border-radius: 999px; background: var(--panel2); color: var(--muted); font-size: 12px; }
.ok { color: var(--good); }
.bad { color: var(--bad); }
.warn { color: var(--warn); }
button, select, input {
  background: var(--panel2);
  color: var(--text);
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 8px 10px;
}
button { cursor: pointer; }
button:hover { border-color: var(--accent); }
button.danger:hover { border-color: var(--bad); }
pre {
  margin: 0;
  background: #0b0d0f;
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 12px;
  min-height: 280px;
  max-height: 520px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}
canvas { width: 100%; height: 160px; display: block; }
@media (max-width: 900px) {
  .grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .wide { grid-column: 1 / -1; }
}
@media (max-width: 560px) {
  header { align-items: flex-start; flex-direction: column; }
  main { padding: 14px; }
  .grid { grid-template-columns: 1fr; }
}
</style>
</head>
<body>
<header>
  <div>
    <h1>Minecraft Monitor</h1>
    <div class="sub" id="updated">Loading...</div>
  </div>
  <div class="row">
    <button onclick="refreshAll()">Refresh</button>
  </div>
</header>
<main>
  <section class="grid">
    <div class="panel"><h2>CPU</h2><div class="metric" id="cpu">-</div><div class="sub">Cloud Shell VM</div></div>
    <div class="panel"><h2>Memory</h2><div class="metric" id="mem">-</div><div class="sub" id="memSub">-</div></div>
    <div class="panel"><h2>Disk</h2><div class="metric" id="disk">-</div><div class="sub" id="diskSub">-</div></div>
    <div class="panel"><h2>Players</h2><div class="metric" id="players">-</div><div class="sub" id="playerNames">-</div></div>

    <div class="panel wide">
      <h2>Minecraft</h2>
      <div class="row" id="mcHealth"></div>
      <div class="row">
        <button onclick="act('minecraft','start')">Start</button>
        <button onclick="act('minecraft','stop')">Stop</button>
        <button onclick="act('minecraft','restart')">Restart</button>
        <button class="danger" onclick="act('minecraft','kill')">Kill</button>
      </div>
      <div class="sub" id="mcDetails"></div>
    </div>

    <div class="panel wide">
      <h2>playit</h2>
      <div class="row" id="playitHealth"></div>
      <div class="row">
        <button onclick="act('playit','start')">Start</button>
        <button onclick="act('playit','stop')">Stop</button>
        <button onclick="act('playit','restart')">Restart</button>
        <button class="danger" onclick="act('playit','kill')">Kill</button>
      </div>
      <div class="sub" id="playitAddress"></div>
    </div>

    <div class="panel full">
      <h2>Machine History - Last 7 Days</h2>
      <canvas id="chart" width="1200" height="220"></canvas>
    </div>

    <div class="panel full">
      <div class="row" style="justify-content: space-between; margin-bottom: 10px;">
        <h2 style="margin:0">Logs</h2>
        <div class="row">
          <select id="logTarget" onchange="loadLogs()">
            <option value="server">server latest.log</option>
            <option value="minecraft">supervised minecraft</option>
            <option value="playit">playit</option>
            <option value="supervisor">monitor</option>
          </select>
          <input id="logSearch" placeholder="Search latest 200 lines" oninput="loadLogs()">
        </div>
      </div>
      <pre id="logs">Loading...</pre>
    </div>
  </section>
</main>
<script>
const $ = id => document.getElementById(id);

async function json(url, opts) {
  const res = await fetch(url, opts);
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

function pill(text, good) {
  const cls = good === true ? 'ok' : good === false ? 'bad' : 'warn';
  return '<span class="pill ' + cls + '">' + text + '</span>';
}

async function refreshAll() {
  try {
    const s = await json('/api/status');
    $('updated').textContent = 'Updated ' + new Date(s.generatedAt).toLocaleString();
    $('cpu').textContent = s.machine.cpuPercent.toFixed(1) + '%';
    $('mem').textContent = s.machine.memPercent.toFixed(1) + '%';
    $('memSub').textContent = s.machine.memUsedMb + ' / ' + s.machine.memTotalMb + ' MB';
    $('disk').textContent = s.machine.diskPercent.toFixed(1) + '%';
    $('diskSub').textContent = s.machine.diskUsedMb + ' / ' + s.machine.diskTotalMb + ' MB';
    $('players').textContent = s.minecraft.playersOnline + ' / ' + s.minecraft.maxPlayers;
    const players = Array.isArray(s.minecraft.players) ? s.minecraft.players : [];
    $('playerNames').textContent = players.length ? players.join(', ') : 'No players online';
    $('mcHealth').innerHTML =
      pill(s.processes.minecraft.running ? 'process running' : 'process stopped', s.processes.minecraft.running) +
      pill(s.minecraft.portOpen ? 'port open' : 'port closed', s.minecraft.portOpen) +
      pill(s.minecraft.rconOk ? 'rcon ok' : 'rcon unavailable', s.minecraft.rconOk);
    $('mcDetails').textContent = 'Version ' + (s.minecraft.version || '-') +
      ' | TPS ' + (s.minecraft.tps || '-') +
      ' | MSPT ' + (s.minecraft.mspt || '-') +
      ' | uptime ' + formatDuration(s.processes.minecraft.uptimeSec);
    $('playitHealth').innerHTML =
      pill(s.processes.playit.running ? 'process running' : 'process stopped', s.processes.playit.running) +
      pill(s.playit.socketExists ? 'socket exists' : 'socket missing', s.playit.socketExists);
    $('playitAddress').textContent = s.playit.address ? 'Address: ' + s.playit.address : 'Address not detected in logs yet';
    drawChart(await json('/api/metrics'));
  } catch (e) {
    $('updated').textContent = 'Error: ' + e.message;
  }
}

async function loadLogs() {
  const target = $('logTarget').value;
  const q = encodeURIComponent($('logSearch').value);
  try {
    const data = await json('/api/logs?target=' + target + '&q=' + q);
    $('logs').textContent = data.lines.join('\n');
  } catch (e) {
    $('logs').textContent = e.message;
  }
}

async function act(target, action) {
  if (action === 'kill' && !confirm('Kill ' + target + ' immediately?')) return;
  await json('/api/' + target + '/' + action, { method: 'POST' });
  setTimeout(refreshAll, 1000);
}

function formatDuration(sec) {
  if (!sec) return '-';
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return h + 'h ' + m + 'm';
}

function drawChart(points) {
  const canvas = $('chart');
  const ctx = canvas.getContext('2d');
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  ctx.strokeStyle = '#333941';
  ctx.lineWidth = 1;
  for (let y = 20; y <= 200; y += 45) {
    ctx.beginPath(); ctx.moveTo(30, y); ctx.lineTo(1180, y); ctx.stroke();
  }
  drawLine(ctx, points.map(p => p.cpuPercent), '#63b3ed');
  drawLine(ctx, points.map(p => p.memPercent), '#31c48d');
  drawLine(ctx, points.map(p => p.diskPercent), '#f6ad55');
  ctx.fillStyle = '#9ba7b3';
  ctx.fillText('CPU blue, memory green, disk orange', 34, 214);
}

function drawLine(ctx, values, color) {
  if (!values.length) return;
  ctx.strokeStyle = color;
  ctx.lineWidth = 2;
  ctx.beginPath();
  values.forEach((v, i) => {
    const x = 34 + (i / Math.max(1, values.length - 1)) * 1140;
    const y = 200 - (Math.max(0, Math.min(100, v)) / 100) * 180;
    if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.stroke();
}

refreshAll();
loadLogs();
setInterval(refreshAll, 5000);
setInterval(loadLogs, 10000);
</script>
</body>
</html>`))
