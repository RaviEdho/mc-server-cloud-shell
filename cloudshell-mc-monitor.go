package main

import (
	"archive/zip"
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
	UpdatedAt     string    `json:"updatedAt,omitempty"`
	PortOpen      bool      `json:"portOpen"`
	RCONOK        bool      `json:"rconOk"`
	Version       string    `json:"version"`
	World         worldInfo `json:"world"`
	PlayersOnline int       `json:"playersOnline"`
	MaxPlayers    int       `json:"maxPlayers"`
	Players       []string  `json:"players"`
	TPS           string    `json:"tps"`
	MSPT          string    `json:"mspt"`
	LastError     string    `json:"lastError,omitempty"`
}

type worldInfo struct {
	Name       string `json:"name"`
	GameMode   string `json:"gameMode"`
	Difficulty string `json:"difficulty"`
	Hardcore   bool   `json:"hardcore"`
	SizeBytes  int64  `json:"sizeBytes"`
	Size       string `json:"size"`
}

type playitStatus struct {
	SocketExists bool   `json:"socketExists"`
	Address      string `json:"address"`
	LastError    string `json:"lastError,omitempty"`
}

type commandRequest struct {
	Command string `json:"command"`
}

func main() {
	mode := flag.String("mode", "", "start, daemon, stop, restart-monitor, status, or configure")
	startFlag := flag.Bool("start", false, "start the monitor daemon")
	daemonFlag := flag.Bool("daemon", false, "run the monitor in the foreground")
	stopFlag := flag.Bool("stop", false, "stop the monitor and supervised services")
	statusFlag := flag.Bool("status", false, "print monitor status")
	configureFlag := flag.Bool("configure", false, "reapply server.properties RCON/query settings")
	restartFlag := flag.Bool("restart", false, "restart all services or a target: all, monitor, minecraft, playit")
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		fatal(err)
	}

	action, _, err := resolveCLIAction(*mode, flag.Args(), map[string]bool{
		"start":     *startFlag,
		"daemon":    *daemonFlag,
		"stop":      *stopFlag,
		"status":    *statusFlag,
		"configure": *configureFlag,
		"restart":   *restartFlag,
	})
	if err != nil {
		fatal(err)
	}

	switch action {
	case "start":
		err = startDaemon(cfg)
	case "daemon":
		err = runDaemon(cfg)
	case "stop":
		err = stopDaemon(cfg)
	case "restart-monitor":
		err = restartMonitor(cfg)
	case "restart-all":
		err = restartAll(cfg)
	case "restart-minecraft":
		err = restartManagedService(cfg, "minecraft")
	case "restart-playit":
		err = restartManagedService(cfg, "playit")
	case "status":
		err = status(cfg)
	case "configure":
		err = ensureServerProperties(cfg)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		fatal(err)
	}
}

func resolveCLIAction(mode string, args []string, flags map[string]bool) (string, string, error) {
	selected := 0
	for _, enabled := range flags {
		if enabled {
			selected++
		}
	}
	if mode != "" {
		selected++
	}
	if selected > 1 {
		return "", "", errors.New("choose only one command flag")
	}
	if mode != "" {
		if len(args) > 0 {
			return "", "", fmt.Errorf("-mode %s does not accept extra arguments", mode)
		}
		if mode == "restart-monitor" {
			return "restart-monitor", "", nil
		}
		return mode, "", nil
	}

	switch {
	case flags["start"]:
		return simpleCLIAction("start", args)
	case flags["daemon"]:
		return simpleCLIAction("daemon", args)
	case flags["stop"]:
		return simpleCLIAction("stop", args)
	case flags["status"]:
		return simpleCLIAction("status", args)
	case flags["configure"]:
		return simpleCLIAction("configure", args)
	case flags["restart"]:
		target, err := cliTarget(args, "all")
		if err != nil {
			return "", "", err
		}
		action, err := restartActionForTarget(target)
		return action, target, err
	default:
		if len(args) > 0 {
			return "", "", fmt.Errorf("unknown command argument %q", args[0])
		}
		return "start", "", nil
	}
}

func simpleCLIAction(action string, args []string) (string, string, error) {
	if len(args) > 0 {
		return "", "", fmt.Errorf("-%s does not accept extra arguments", action)
	}
	return action, "", nil
}

func cliTarget(args []string, fallback string) (string, error) {
	if len(args) == 0 {
		return fallback, nil
	}
	if len(args) > 1 {
		return "", fmt.Errorf("expected one restart target, got %d", len(args))
	}
	return strings.ToLower(strings.TrimSpace(args[0])), nil
}

func restartActionForTarget(target string) (string, error) {
	switch target {
	case "", "all":
		return "restart-all", nil
	case "mon", "monitor", "web":
		return "restart-monitor", nil
	case "minecraft", "mc", "server":
		return "restart-minecraft", nil
	case "playit", "conn", "connection":
		return "restart-playit", nil
	default:
		return "", fmt.Errorf("unknown restart target %q", target)
	}
}

func loadConfig() (config, error) {
	exe, err := os.Executable()
	if err != nil {
		return config{}, err
	}
	root := os.Getenv("MC_MONITOR_ROOT")
	if root == "" {
		root = os.Getenv("MC_AUTOSTART_ROOT")
	}
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

	cmd := exec.Command(exe, "-daemon")
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
	mux.HandleFunc("/api/command", a.handleCommand)
	mux.HandleFunc("/api/world/download", a.handleWorldDownload)
	mux.HandleFunc("/api/world/upload", a.handleWorldUpload)
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
	w.Header().Set("Cache-Control", "no-store")
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
	path := a.logPath(target)
	lines, err := tailLines(path, 200, search)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"target": target, "lines": lines})
}

func (a *app) logPath(target string) string {
	switch target {
	case "server", "minecraft":
		return filepath.Join(a.cfg.runtimeDir, "minecraft.log")
	case "playit":
		return filepath.Join(a.cfg.runtimeDir, "playit.log")
	case "latest":
		return filepath.Join(a.cfg.serverDir, "logs", "latest.log")
	default:
		return filepath.Join(a.cfg.runtimeDir, "supervisor.log")
	}
}

func (a *app) handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req commandRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid command request", http.StatusBadRequest)
		return
	}
	command := strings.TrimSpace(req.Command)
	command = strings.TrimPrefix(command, "/")
	command = strings.TrimSpace(command)
	if command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	if len(command) > 1024 {
		http.Error(w, "command is too long", http.StatusBadRequest)
		return
	}
	rconCommand := dashboardRCONCommand(command)
	out, err := a.rconCommand(rconCommand)
	if err != nil {
		writeError(w, err)
		return
	}
	if rconCommand != command {
		a.log("dashboard command: %s -> %s", command, rconCommand)
	} else {
		a.log("dashboard command: %s", command)
	}
	writeJSON(w, map[string]string{"command": command, "output": out})
}

func (a *app) handleWorldDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	worldPath, world, err := a.worldPath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(worldPath)
	if err != nil || !info.IsDir() {
		http.Error(w, "world folder not found", http.StatusNotFound)
		return
	}
	filename := safeZipName(world.Name)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
	if err := zipWorld(worldPath, world.Name, w); err != nil {
		a.log("world download failed: %v", err)
	}
}

func (a *app) handleWorldUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.minecraft.snapshot().Running {
		http.Error(w, "stop Minecraft before uploading a world zip", http.StatusConflict)
		return
	}
	worldPath, world, err := a.worldPath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "invalid zip upload", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("world")
	if err != nil {
		http.Error(w, "world zip file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if !strings.EqualFold(filepath.Ext(header.Filename), ".zip") {
		http.Error(w, "only .zip files are accepted", http.StatusBadRequest)
		return
	}

	tmpDir, err := os.MkdirTemp(a.cfg.runtimeDir, "world-upload-*")
	if err != nil {
		writeError(w, err)
		return
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "upload.zip")
	tmpZip, err := os.Create(zipPath)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := io.Copy(tmpZip, file); err != nil {
		_ = tmpZip.Close()
		writeError(w, err)
		return
	}
	if err := tmpZip.Close(); err != nil {
		writeError(w, err)
		return
	}

	extractDir := filepath.Join(tmpDir, "extract")
	sourceDir, err := extractWorldZip(zipPath, extractDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := replaceWorldDir(sourceDir, worldPath); err != nil {
		writeError(w, err)
		return
	}
	a.log("world uploaded: %s -> %s", header.Filename, world.Name)
	writeJSON(w, map[string]string{"ok": "true", "world": world.Name})
}

func dashboardRCONCommand(command string) string {
	name, message, ok := strings.Cut(command, " ")
	if !ok || !strings.EqualFold(name, "say") {
		return command
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return command
	}
	payload, err := json.Marshal(map[string]string{"text": "[Server] " + message})
	if err != nil {
		return command
	}
	return "tellraw @a " + string(payload)
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

func (a *app) worldPath() (string, worldInfo, error) {
	propsPath := filepath.Join(a.cfg.serverDir, "server.properties")
	world := readWorldInfo(propsPath)
	if world.Name == "" {
		world.Name = "world"
	}
	if filepath.IsAbs(world.Name) {
		return "", world, fmt.Errorf("world name must be relative")
	}
	cleanName := filepath.Clean(world.Name)
	if cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
		return "", world, fmt.Errorf("world name escapes server directory")
	}
	path := filepath.Join(a.cfg.serverDir, cleanName)
	serverDir, err := filepath.Abs(a.cfg.serverDir)
	if err != nil {
		return "", world, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", world, err
	}
	rel, err := filepath.Rel(serverDir, absPath)
	if err != nil {
		return "", world, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", world, fmt.Errorf("world path escapes server directory")
	}
	return absPath, world, nil
}

func safeZipName(worldName string) string {
	name := filepath.Base(filepath.Clean(worldName))
	name = strings.TrimSpace(name)
	if name == "." || name == "" {
		name = "world"
	}
	return name + ".zip"
}

func zipWorld(worldPath, worldName string, w io.Writer) error {
	zw := zip.NewWriter(w)
	defer zw.Close()
	rootName := filepath.Base(filepath.Clean(worldName))
	if rootName == "." || rootName == "" {
		rootName = "world"
	}
	return filepath.WalkDir(worldPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(worldPath, path)
		if err != nil {
			return err
		}
		zipName := rootName
		if rel != "." {
			zipName = filepath.ToSlash(filepath.Join(rootName, rel))
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = zipName
		if d.IsDir() {
			header.Name += "/"
			_, err = zw.CreateHeader(header)
			return err
		}
		header.Method = zip.Deflate
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func extractWorldZip(zipPath, dest string) (string, error) {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("invalid zip file")
	}
	defer reader.Close()
	if len(reader.File) == 0 {
		return "", fmt.Errorf("zip file is empty")
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return "", err
	}
	for _, file := range reader.File {
		cleanName, err := cleanZipPath(file.Name)
		if err != nil {
			return "", err
		}
		if file.FileInfo().Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("zip file contains unsupported symlink: %s", file.Name)
		}
		target := filepath.Join(dest, cleanName)
		if !pathInside(dest, target) {
			return "", fmt.Errorf("zip file contains unsafe path: %s", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, file.FileInfo().Mode()); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return "", err
		}
		src, err := file.Open()
		if err != nil {
			return "", err
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, file.FileInfo().Mode())
		if err != nil {
			_ = src.Close()
			return "", err
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		_ = src.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	source, err := uploadedWorldRoot(dest)
	if err != nil {
		return "", err
	}
	if !fileExists(filepath.Join(source, "level.dat")) {
		return "", fmt.Errorf("zip does not contain a Minecraft world level.dat")
	}
	return source, nil
}

func cleanZipPath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("zip file contains unsafe path: %s", name)
	}
	return clean, nil
}

func uploadedWorldRoot(extractDir string) (string, error) {
	if fileExists(filepath.Join(extractDir, "level.dat")) {
		return extractDir, nil
	}
	entries, err := os.ReadDir(extractDir)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, entry := range entries {
		name := entry.Name()
		if name == "__MACOSX" || name == ".DS_Store" {
			continue
		}
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(extractDir, name))
			continue
		}
		return "", fmt.Errorf("zip must contain a single world folder or world files directly")
	}
	if len(dirs) != 1 {
		return "", fmt.Errorf("zip must contain a single world folder or world files directly")
	}
	return dirs[0], nil
}

func replaceWorldDir(source, target string) error {
	if !pathInside(filepath.Dir(target), target) {
		return fmt.Errorf("unsafe world target")
	}
	backup := target + ".backup-" + time.Now().Format("20060102-150405")
	hadExisting := false
	if _, err := os.Stat(target); err == nil {
		hadExisting = true
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		if hadExisting {
			_ = os.Rename(backup, target)
		}
		return err
	}
	if err := os.Rename(source, target); err != nil {
		if hadExisting {
			_ = os.Rename(backup, target)
		}
		return err
	}
	if hadExisting {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func pathInside(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	propsPath := filepath.Join(a.cfg.serverDir, "server.properties")
	if a.minecraftCache.UpdatedAt != "" {
		return a.minecraftCache
	}
	return minecraftStatus{
		UpdatedAt:  time.Now().Format(time.RFC3339),
		PortOpen:   dialOK("127.0.0.1:25565", 700*time.Millisecond),
		Version:    parseServerVersion(filepath.Join(a.cfg.runtimeDir, "minecraft.log"), filepath.Join(a.cfg.serverDir, "logs", "latest.log")),
		World:      readWorldInfo(propsPath),
		MaxPlayers: readMaxPlayers(propsPath),
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
	propsPath := filepath.Join(a.cfg.serverDir, "server.properties")
	result := minecraftStatus{
		PortOpen: dialOK("127.0.0.1:25565", 700*time.Millisecond),
		Version:  parseServerVersion(filepath.Join(a.cfg.runtimeDir, "minecraft.log"), filepath.Join(a.cfg.serverDir, "logs", "latest.log")),
		World:    readWorldInfo(propsPath),
		Players:  []string{},
	}
	out, err := a.rconCommands("list", "tick query")
	if err != nil {
		result.LastError = err.Error()
		result.MaxPlayers = readMaxPlayers(propsPath)
		return result
	}
	result.RCONOK = true
	result.PlayersOnline, result.MaxPlayers, result.Players = parseListOutput(out[0])
	if result.MaxPlayers == 0 {
		result.MaxPlayers = readMaxPlayers(propsPath)
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

func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

func formatBytes(size int64) string {
	if size <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(size)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", size, units[unit])
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
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
		return readCachedPlayitAddress(filepath.Join(filepath.Dir(path), "playit.address"))
	}
	content := string(data)
	if host := preferredPlayitHost(findPlayitHosts(content)); host != "" {
		cachePlayitAddress(filepath.Join(filepath.Dir(path), "playit.address"), host)
		return host
	}

	var decodedHosts []string
	tokenRe := regexp.MustCompile(`token:\s*([0-9a-fA-F]+)`)
	for _, match := range tokenRe.FindAllStringSubmatch(content, -1) {
		decoded, err := hex.DecodeString(match[1])
		if err != nil {
			continue
		}
		decodedHosts = append(decodedHosts, findPlayitHosts(string(decoded))...)
	}
	if host := preferredPlayitHost(decodedHosts); host != "" {
		cachePlayitAddress(filepath.Join(filepath.Dir(path), "playit.address"), host)
		return host
	}
	return readCachedPlayitAddress(filepath.Join(filepath.Dir(path), "playit.address"))
}

func findPlayitHosts(content string) []string {
	re := regexp.MustCompile(`(?i)[a-z0-9][a-z0-9.-]*\.(?:joinmc\.link|playit\.gg)(?::\d+)?`)
	return re.FindAllString(content, -1)
}

func preferredPlayitHost(hosts []string) string {
	var fallback string
	var joinMC string
	for _, host := range hosts {
		host = strings.Trim(host, ".")
		if host == "" {
			continue
		}
		fallback = host
		if strings.Contains(strings.ToLower(host), ".joinmc.link") {
			joinMC = host
		}
	}
	if joinMC != "" {
		return joinMC
	}
	return fallback
}

func cachePlayitAddress(path, address string) {
	if address == "" {
		return
	}
	_ = os.WriteFile(path, []byte(address+"\n"), 0644)
}

func readCachedPlayitAddress(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return preferredPlayitHost(findPlayitHosts(strings.TrimSpace(string(data))))
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

func readWorldInfo(path string) worldInfo {
	serverDir := filepath.Dir(path)
	data, err := os.ReadFile(path)
	if err != nil {
		info := worldInfo{Name: "world"}
		info.SizeBytes = dirSize(filepath.Join(serverDir, info.Name))
		info.Size = formatBytes(info.SizeBytes)
		return info
	}
	props := parseProperties(string(data))
	name := defaultString(props["level-name"], "world")
	sizeBytes := dirSize(filepath.Join(serverDir, name))
	return worldInfo{
		Name:       name,
		GameMode:   defaultString(props["gamemode"], "survival"),
		Difficulty: defaultString(props["difficulty"], "easy"),
		Hardcore:   strings.EqualFold(props["hardcore"], "true"),
		SizeBytes:  sizeBytes,
		Size:       formatBytes(sizeBytes),
	}
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
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
		if logLineMatches(line, search) {
			filtered = append(filtered, line)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, nil
}

func logLineMatches(line string, search string) bool {
	if search == "" {
		return true
	}
	return strings.Contains(strings.ToLower(line), strings.ToLower(search))
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
	if waitForProcessExit(pid, 8*time.Second) {
		_ = os.Remove(filepath.Join(cfg.runtimeDir, pidFileName))
		return startDaemon(cfg)
	}

	fmt.Printf("Monitor PID %d did not exit after handoff signal; killing only the monitor process.\n", pid)
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	if waitForProcessExit(pid, 8*time.Second) {
		_ = os.Remove(filepath.Join(cfg.runtimeDir, pidFileName))
		return startDaemon(cfg)
	}
	return fmt.Errorf("monitor PID %d did not exit after SIGKILL", pid)
}

func restartAll(cfg config) error {
	pid, err := readPID(cfg)
	if err != nil {
		fmt.Println("Monitor is not running; starting it.")
		return startDaemon(cfg)
	}
	if processRunning(pid) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return err
		}
		fmt.Printf("Sent restart signal to monitor PID %d.\n", pid)
		if !waitForProcessExit(pid, 60*time.Second) {
			return fmt.Errorf("monitor PID %d did not exit after restart signal", pid)
		}
	}
	_ = os.Remove(filepath.Join(cfg.runtimeDir, pidFileName))
	return startDaemon(cfg)
}

func restartManagedService(cfg config, target string) error {
	url := monitorActionURL(cfg, target, "restart")
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("restart %s through monitor failed: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restart %s through monitor failed: %s: %s", target, resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Printf("Requested %s restart through monitor.\n", target)
	return nil
}

func monitorActionURL(cfg config, target string, action string) string {
	host, port, err := net.SplitHostPort(cfg.webAddr)
	if err != nil {
		host = "127.0.0.1"
		port = "8080"
	} else if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/" + target + "/" + action
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
	if processZombie(pid) {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processRunning(pid) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return !processRunning(pid)
}

func processZombie(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "State:") {
			return strings.Contains(line, "Z")
		}
	}
	return false
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
  --bg: #0e1116;
  --panel: #171b22;
  --panel2: #202633;
  --text: #e8ecef;
  --muted: #9aa6b2;
  --line: #2b3340;
  --good: #36d399;
  --warn: #fbbf24;
  --bad: #fb7185;
  --accent: #38bdf8;
  --accent2: #a78bfa;
  --shadow: 0 18px 50px rgba(0,0,0,0.22);
}
* { box-sizing: border-box; }
body {
  margin: 0;
  background:
    linear-gradient(180deg, rgba(56,189,248,0.08), transparent 280px),
    var(--bg);
  color: var(--text);
  font: 14px/1.4 system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 18px 32px;
  background: rgba(14,17,22,0.82);
  backdrop-filter: blur(14px);
  border-bottom: 1px solid var(--line);
  position: sticky;
  top: 0;
  z-index: 10;
}
h1 { margin: 0; font-size: 20px; font-weight: 650; }
main { padding: 20px 32px 30px; max-width: 1120px; margin: 0 auto; }
.grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; }
.wide { grid-column: span 2; }
.full { grid-column: 1 / -1; }
.status-layout {
  display: grid;
  grid-template-columns: minmax(280px, 0.9fr) minmax(0, 1.25fr);
  gap: 12px;
  align-items: stretch;
  margin-bottom: 12px;
}
.service-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}
.service-grid .panel,
.machine-panel { min-height: 0; }
.info-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
  margin-bottom: 12px;
}
.panel {
  background: rgba(23,27,34,0.94);
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 14px;
  box-shadow: var(--shadow);
}
.panel h2 { margin: 0 0 12px; font-size: 13px; color: var(--muted); font-weight: 650; text-transform: uppercase; letter-spacing: 0; }
.metric { font-size: 26px; font-weight: 700; }
.metric.wrap { overflow-wrap: anywhere; line-height: 1.15; }
.sub { color: var(--muted); font-size: 12px; margin-top: 4px; }
.row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.row + .row { margin-top: 8px; }
.pill { padding: 3px 8px; border-radius: 999px; background: var(--panel2); color: var(--muted); font-size: 12px; border: 1px solid rgba(255,255,255,0.04); }
.ok { color: var(--good); }
.bad { color: var(--bad); }
.warn { color: var(--warn); }
.usage-list { display: grid; gap: 16px; }
.usage-head { display: flex; justify-content: space-between; gap: 10px; align-items: baseline; }
.usage-label { color: var(--muted); font-weight: 600; }
.usage-value { font-size: 18px; font-weight: 700; }
.usage-detail { color: var(--muted); font-size: 12px; margin-top: 4px; }
.bar {
  height: 8px;
  margin-top: 8px;
  overflow: hidden;
  background: #0b0d0f;
  border: 1px solid var(--line);
  border-radius: 999px;
}
.bar > span {
  display: block;
  height: 100%;
  width: 0;
  border-radius: inherit;
  background: var(--accent);
  transition: width 180ms ease;
}
.bar.mem > span { background: var(--good); }
.bar.disk > span { background: var(--warn); }
button, select, input {
  background: var(--panel2);
  color: var(--text);
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 8px 10px;
}
button { cursor: pointer; transition: border-color 150ms ease, transform 150ms ease, background 150ms ease; }
button:disabled { cursor: not-allowed; opacity: 0.55; }
button:hover { border-color: var(--accent); transform: translateY(-1px); }
button.danger:hover { border-color: var(--bad); }
.hidden-file { display: none; }
.panel-title-row {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  align-items: center;
  margin-bottom: 10px;
}
.panel-title-row h2 { margin: 0; }
.segmented {
  display: inline-flex;
  gap: 2px;
  padding: 3px;
  background: #0b0f16;
  border: 1px solid var(--line);
  border-radius: 8px;
}
.segmented button {
  border: 0;
  background: transparent;
  color: var(--muted);
  padding: 5px 9px;
}
.segmented button:hover,
.segmented button.active {
  color: var(--text);
  background: var(--panel2);
  transform: none;
}
.chart-panel { padding-bottom: 10px; }
.chart-meta {
  min-height: 18px;
  margin: -4px 0 8px;
}
.chart-wrap {
  height: 260px;
  min-height: 260px;
  position: relative;
}
.terminal {
  display: flex;
  gap: 8px;
  margin-top: 10px;
}
.terminal input {
  flex: 1;
  min-width: 220px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
.command-output {
  display: none;
  margin-top: 8px;
  border: 1px solid var(--line);
  border-radius: 6px;
  background: #0b0d0f;
  color: var(--text);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  max-height: 120px;
  overflow: auto;
  padding: 6px;
  white-space: pre-wrap;
  word-break: break-word;
}
.command-response {
  color: var(--muted);
  padding: 4px 6px;
}
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
canvas { width: 100%; display: block; }
@media (max-width: 900px) {
  .grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .wide { grid-column: 1 / -1; }
  .status-layout { grid-template-columns: 1fr; }
}
@media (max-width: 560px) {
  header { align-items: flex-start; flex-direction: column; }
  main { padding: 14px; }
  .grid { grid-template-columns: 1fr; }
  .service-grid,
  .info-grid { grid-template-columns: 1fr; }
  .panel-title-row { align-items: flex-start; flex-direction: column; }
  .chart-wrap { height: 240px; min-height: 240px; }
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
  <section class="status-layout">
    <div class="panel machine-panel">
      <h2>Machine Status</h2>
      <div class="usage-list">
        <div>
          <div class="usage-head"><span class="usage-label">CPU</span><span class="usage-value" id="cpu">-</span></div>
          <div class="usage-detail">Cloud Shell VM</div>
          <div class="bar cpu"><span id="cpuBar"></span></div>
        </div>
        <div>
          <div class="usage-head"><span class="usage-label">Memory</span><span class="usage-value" id="mem">-</span></div>
          <div class="usage-detail" id="memSub">-</div>
          <div class="bar mem"><span id="memBar"></span></div>
        </div>
        <div>
          <div class="usage-head"><span class="usage-label">Disk</span><span class="usage-value" id="disk">-</span></div>
          <div class="usage-detail" id="diskSub">-</div>
          <div class="bar disk"><span id="diskBar"></span></div>
        </div>
      </div>
    </div>

    <div class="service-grid">
      <div class="panel">
        <h2>Minecraft</h2>
        <div class="row" id="mcHealth"></div>
        <div class="row">
          <button id="minecraftPower" onclick="powerAct('minecraft')">Start</button>
          <button onclick="act('minecraft','restart')">Restart</button>
          <button class="danger" onclick="act('minecraft','kill')">Kill</button>
        </div>
        <div class="sub" id="mcDetails"></div>
      </div>

      <div class="panel">
        <h2>playit</h2>
        <div class="row" id="playitHealth"></div>
        <div class="row">
          <button id="playitPower" onclick="powerAct('playit')">Start</button>
          <button onclick="act('playit','restart')">Restart</button>
          <button class="danger" onclick="act('playit','kill')">Kill</button>
        </div>
        <div class="sub" id="playitAddress"></div>
      </div>
    </div>
  </section>

  <section class="info-grid">
    <div class="panel"><h2>Players</h2><div class="metric" id="players">-</div><div class="sub" id="playerNames">-</div></div>
    <div class="panel">
      <h2>Minecraft World</h2>
      <div class="metric wrap" id="worldName">-</div>
      <div class="sub" id="worldDetails">-</div>
      <div class="row">
        <button onclick="downloadWorld()">Download ZIP</button>
        <button id="worldUploadButton" onclick="$('worldUpload').click()">Upload ZIP</button>
        <input class="hidden-file" id="worldUpload" type="file" accept=".zip,application/zip" onchange="uploadWorld()">
      </div>
      <div class="sub" id="worldTransfer"></div>
    </div>
  </section>

  <section class="grid">
    <div class="panel full chart-panel">
      <div class="panel-title-row">
        <h2>Machine History</h2>
        <div class="segmented" id="chartRanges">
          <button type="button" data-hours="1">1h</button>
          <button type="button" data-hours="6">6h</button>
          <button type="button" data-hours="24">24h</button>
          <button type="button" data-hours="168" class="active">7d</button>
        </div>
      </div>
      <div class="sub chart-meta" id="chartAvailability">Calculating availability...</div>
      <div class="chart-wrap">
        <canvas id="chart"></canvas>
      </div>
    </div>

    <div class="panel full">
      <div class="row" style="justify-content: space-between; margin-bottom: 10px;">
        <h2 style="margin:0">Logs</h2>
        <div class="row">
          <select id="logTarget" onchange="startLogPolling()">
            <option value="server">server live</option>
            <option value="latest">server latest.log</option>
            <option value="playit">playit</option>
            <option value="supervisor">monitor</option>
          </select>
          <input id="logSearch" placeholder="Search latest 200 lines" oninput="queueLogPollingRestart()">
        </div>
      </div>
      <pre id="logs">Loading...</pre>
      <form class="terminal" onsubmit="sendCommand(event)">
        <input id="commandInput" autocomplete="off" spellcheck="false" placeholder="/say hello">
        <button id="commandSend" type="submit">Send</button>
      </form>
      <div class="command-output" id="commandOutput"></div>
    </div>
  </section>
</main>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.min.js"></script>
<script>
const $ = id => document.getElementById(id);
const maxLogLines = 200;
let logLines = [];
let logPollTimer = null;
let logPollInFlight = 0;
let logPollGeneration = 0;
let logRestartTimer = null;
const commandStorageKey = 'mc-command-history';
let commandHistory = loadCommandHistory();
let commandHistoryIndex = commandHistory.length;
let metricChart = null;
let metricPoints = [];
let visibleMetricPoints = [];
let visibleDowntime = [];
let chartRangeHours = 168;
const metricSampleMs = 30 * 1000;
const downtimeGapMs = 90 * 1000;
const browserTimeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
const browserTimeZoneLabel = browserTimeZone || 'browser local time';

async function json(url, opts) {
  const res = await fetch(url, opts);
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

function pill(text, good) {
  const cls = good === true ? 'ok' : good === false ? 'bad' : 'warn';
  return '<span class="pill ' + cls + '">' + text + '</span>';
}

function setUsage(valueId, barId, percent) {
  const safe = Math.max(0, Math.min(100, Number(percent) || 0));
  $(valueId).textContent = safe.toFixed(1) + '%';
  $(barId).style.width = safe + '%';
}

function setPowerButton(target, running) {
  const button = $(target + 'Power');
  button.dataset.action = running ? 'stop' : 'start';
  button.textContent = running ? 'Stop' : 'Start';
  button.classList.toggle('danger', running);
}

async function refreshAll() {
  try {
    const s = await json('/api/status');
    $('updated').textContent = 'Updated ' + new Date(s.generatedAt).toLocaleString();
    setUsage('cpu', 'cpuBar', s.machine.cpuPercent);
    setUsage('mem', 'memBar', s.machine.memPercent);
    $('memSub').textContent = s.machine.memUsedMb + ' / ' + s.machine.memTotalMb + ' MB';
    setUsage('disk', 'diskBar', s.machine.diskPercent);
    $('diskSub').textContent = s.machine.diskUsedMb + ' / ' + s.machine.diskTotalMb + ' MB';
    $('players').textContent = s.minecraft.playersOnline + ' / ' + s.minecraft.maxPlayers;
    const players = Array.isArray(s.minecraft.players) ? s.minecraft.players : [];
    $('playerNames').textContent = players.length ? players.join(', ') : 'No players online';
    const world = s.minecraft.world || {};
    $('worldName').textContent = world.name || '-';
    $('worldDetails').textContent = [
      world.size ? 'Size ' + world.size : '',
      world.gameMode ? 'Mode ' + world.gameMode : '',
      world.difficulty ? 'Difficulty ' + world.difficulty : '',
      world.hardcore ? 'Hardcore' : 'Hardcore off'
    ].filter(Boolean).join(' | ');
    setPowerButton('minecraft', s.processes.minecraft.running);
    setPowerButton('playit', s.processes.playit.running);
    $('worldUploadButton').disabled = s.processes.minecraft.running;
    $('worldUploadButton').title = s.processes.minecraft.running ? 'Stop Minecraft before uploading a world zip' : '';
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
    $('playitAddress').textContent = s.playit.address ? 'Server: ' + s.playit.address : 'Server address not detected in logs yet';
    drawChart(await json('/api/metrics'));
  } catch (e) {
    $('updated').textContent = 'Error: ' + e.message;
  }
}

async function loadLogs(forceBottom, generation) {
  if (logPollInFlight === generation) return;
  logPollInFlight = generation;
  const target = $('logTarget').value;
  const q = encodeURIComponent($('logSearch').value);
  try {
    const data = await json('/api/logs?target=' + target + '&q=' + q + '&_=' + Date.now(), { cache: 'no-store' });
    if (generation !== logPollGeneration) return;
    const nextLines = Array.isArray(data.lines) ? data.lines.slice(-maxLogLines) : [];
    const nextPayload = nextLines.join('\n');
    if (forceBottom || nextPayload !== logLines.join('\n')) {
      logLines = nextLines;
      renderLogs(forceBottom);
    }
  } catch (e) {
    if (generation === logPollGeneration) {
      $('logs').textContent = e.message;
    }
  } finally {
    if (logPollInFlight === generation) {
      logPollInFlight = 0;
    }
  }
}

function queueLogPollingRestart() {
  clearTimeout(logRestartTimer);
  logRestartTimer = setTimeout(startLogPolling, 250);
}

function startLogPolling() {
  clearTimeout(logRestartTimer);
  clearInterval(logPollTimer);
  logPollGeneration++;
  logPollInFlight = 0;
  const generation = logPollGeneration;
  loadLogs(true, generation);
  logPollTimer = setInterval(() => loadLogs(false, generation), 250);
}

function renderLogs(forceBottom) {
  const el = $('logs');
  const pinned = el.scrollTop + el.clientHeight >= el.scrollHeight - 24;
  el.textContent = logLines.join('\n');
  if (forceBottom || pinned) {
    el.scrollTop = el.scrollHeight;
  }
}

function loadCommandHistory() {
  try {
    const parsed = JSON.parse(localStorage.getItem(commandStorageKey) || '[]');
    return Array.isArray(parsed) ? parsed.filter(v => typeof v === 'string').slice(-50) : [];
  } catch (_) {
    return [];
  }
}

function saveCommandHistory() {
  localStorage.setItem(commandStorageKey, JSON.stringify(commandHistory.slice(-50)));
}

function rememberCommand(command) {
  commandHistory = commandHistory.filter(item => item !== command);
  commandHistory.push(command);
  commandHistory = commandHistory.slice(-50);
  commandHistoryIndex = commandHistory.length;
  saveCommandHistory();
}

function escapeHTML(value) {
  return value.replace(/[&<>"']/g, ch => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;'
  }[ch]));
}

function handleCommandKey(event) {
  const input = event.currentTarget;
  if (event.key === 'ArrowUp') {
    if (commandHistory.length === 0) return;
    event.preventDefault();
    commandHistoryIndex = Math.max(0, commandHistoryIndex - 1);
    input.value = commandHistory[commandHistoryIndex] || '';
    input.setSelectionRange(input.value.length, input.value.length);
  } else if (event.key === 'ArrowDown') {
    if (commandHistory.length === 0) return;
    event.preventDefault();
    commandHistoryIndex = Math.min(commandHistory.length, commandHistoryIndex + 1);
    input.value = commandHistory[commandHistoryIndex] || '';
    input.setSelectionRange(input.value.length, input.value.length);
  }
}

async function sendCommand(event) {
  event.preventDefault();
  const input = $('commandInput');
  const button = $('commandSend');
  const output = $('commandOutput');
  const command = input.value.trim();
  if (!command) return;

  input.disabled = true;
  button.disabled = true;
  output.style.display = 'block';
  output.innerHTML = '<div class="command-response">&gt; ' + escapeHTML(command) + '</div>';
  try {
    const data = await json('/api/command', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ command })
    });
    const body = data.output ? data.output : '(no output)';
    output.innerHTML = '<div class="command-response">&gt; ' + escapeHTML(data.command) + '\n' + escapeHTML(body) + '</div>';
    rememberCommand(command);
    input.value = '';
    loadLogs(false, logPollGeneration);
  } catch (e) {
    output.innerHTML = '<div class="command-response">Error: ' + escapeHTML(e.message) + '</div>';
  } finally {
    input.disabled = false;
    button.disabled = false;
    input.focus();
  }
}

async function act(target, action) {
  if (action === 'kill' && !confirm('Kill ' + target + ' immediately?')) return;
  await json('/api/' + target + '/' + action, { method: 'POST' });
  setTimeout(refreshAll, 1000);
}

async function powerAct(target) {
  const button = $(target + 'Power');
  await act(target, button.dataset.action || 'start');
}

function downloadWorld() {
  window.location.href = '/api/world/download';
}

async function uploadWorld() {
  const input = $('worldUpload');
  const button = $('worldUploadButton');
  const output = $('worldTransfer');
  const file = input.files && input.files[0];
  if (!file) return;
  if (!file.name.toLowerCase().endsWith('.zip')) {
    output.textContent = 'Only .zip files are accepted';
    input.value = '';
    return;
  }
  const data = new FormData();
  data.append('world', file);
  button.disabled = true;
  output.textContent = 'Uploading ' + file.name + '...';
  try {
    const res = await fetch('/api/world/upload', { method: 'POST', body: data });
    if (!res.ok) throw new Error(await res.text());
    output.textContent = 'World uploaded';
  } catch (e) {
    output.textContent = e.message.trim();
  } finally {
    input.value = '';
    await refreshAll();
  }
}

function formatDuration(sec) {
  if (!sec) return '-';
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return h + 'h ' + m + 'm';
}

function drawChart(points) {
  metricPoints = Array.isArray(points) ? points : [];
  renderMetricChart();
}

function renderMetricChart() {
  if (!window.Chart) {
    $('updated').textContent = 'Chart library failed to load';
    return;
  }
  const canvas = $('chart');
  const rangeEnd = Date.now();
  const rangeStart = rangeEnd - chartRangeHours * 3600 * 1000;
  const parsed = metricPoints.map(parseMetricPoint).filter(Boolean).sort((a, b) => a.ts - b.ts);
  visibleMetricPoints = parsed.filter(p => p.ts >= rangeStart && p.ts <= rangeEnd);
  visibleDowntime = downtimeIntervals(parsed, rangeStart, rangeEnd);
  renderAvailability(rangeStart, rangeEnd, visibleMetricPoints, visibleDowntime);

  const datasets = [
    chartDataset('CPU', seriesWithGaps(visibleMetricPoints, 'cpuPercent'), '#38bdf8', 'rgba(56,189,248,0.14)'),
    chartDataset('Memory', seriesWithGaps(visibleMetricPoints, 'memPercent'), '#36d399', 'rgba(54,211,153,0.12)'),
    chartDataset('Disk', seriesWithGaps(visibleMetricPoints, 'diskPercent'), '#fbbf24', 'rgba(251,191,36,0.10)')
  ];

  if (!metricChart) {
    metricChart = new Chart(canvas, {
      type: 'line',
      data: { datasets },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        normalized: true,
        parsing: false,
        interaction: { mode: 'index', intersect: false },
        plugins: {
          legend: {
            labels: { color: '#d8dee6', usePointStyle: true, boxWidth: 8, boxHeight: 8 }
          },
          tooltip: {
            backgroundColor: '#0b0f16',
            borderColor: '#2b3340',
            borderWidth: 1,
            titleColor: '#e8ecef',
            bodyColor: '#d8dee6',
            displayColors: true,
            callbacks: {
              title: items => {
                const point = items[0] && items[0].raw;
                return point && point.x ? formatBrowserDateTime(point.x) : '';
              },
              label: item => item.dataset.label + ': ' + Number(item.parsed.y || 0).toFixed(1) + '%'
            }
          }
        },
        scales: {
          x: {
            type: 'linear',
            min: rangeStart,
            max: rangeEnd,
            grid: { color: 'rgba(154,166,178,0.08)', drawBorder: false },
            ticks: {
              color: '#9aa6b2',
              maxRotation: 0,
              autoSkip: true,
              maxTicksLimit: 8,
              callback: value => formatMetricTime(Number(value))
            }
          },
          y: {
            min: 0,
            max: 100,
            grid: { color: 'rgba(154,166,178,0.12)', drawBorder: false },
            ticks: { color: '#9aa6b2', callback: value => value + '%' }
          }
        },
        elements: {
          point: { radius: 0, hoverRadius: 4, hitRadius: 12 },
          line: { borderWidth: 2 }
        }
      },
      plugins: [downtimeBandsPlugin]
    });
    return;
  }

  metricChart.options.scales.x.min = rangeStart;
  metricChart.options.scales.x.max = rangeEnd;
  metricChart.data.datasets.forEach((dataset, i) => {
    dataset.data = datasets[i].data;
  });
  metricChart.update('none');
}

function chartDataset(label, data, borderColor, backgroundColor) {
  return {
    label,
    data,
    borderColor,
    backgroundColor,
    fill: true,
    spanGaps: false,
    tension: 0.32
  };
}

function formatMetricTime(value) {
  const date = new Date(value);
  if (chartRangeHours <= 24) {
    return date.toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      timeZone: browserTimeZone
    });
  }
  return date.toLocaleDateString([], {
    month: 'short',
    day: 'numeric',
    timeZone: browserTimeZone
  });
}

function formatBrowserDateTime(value) {
  return new Date(value).toLocaleString([], {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    timeZoneName: 'short',
    timeZone: browserTimeZone
  });
}

function parseMetricPoint(point) {
  const ts = Date.parse(point.time);
  if (!Number.isFinite(ts)) return null;
  return {
    ts,
    cpuPercent: Number(point.cpuPercent) || 0,
    memPercent: Number(point.memPercent) || 0,
    diskPercent: Number(point.diskPercent) || 0
  };
}

function seriesWithGaps(points, key) {
  const data = [];
  let previous = null;
  for (const point of points) {
    if (previous && point.ts - previous.ts > downtimeGapMs) {
      data.push({ x: previous.ts + metricSampleMs, y: null });
      data.push({ x: point.ts - metricSampleMs, y: null });
    }
    data.push({ x: point.ts, y: point[key] });
    previous = point;
  }
  return data;
}

function downtimeIntervals(points, rangeStart, rangeEnd) {
  const intervals = [];
  let previous = null;
  let sawPointInRange = false;

  for (const point of points) {
    if (point.ts < rangeStart) {
      previous = point;
      continue;
    }
    if (point.ts > rangeEnd) break;

    sawPointInRange = true;
    if (previous && point.ts - previous.ts > downtimeGapMs) {
      intervals.push({
        start: Math.max(rangeStart, previous.ts + metricSampleMs),
        end: Math.min(rangeEnd, point.ts - metricSampleMs)
      });
    } else if (!previous && point.ts - rangeStart > downtimeGapMs) {
      intervals.push({ start: rangeStart, end: point.ts - metricSampleMs });
    }
    previous = point;
  }

  if (previous && rangeEnd - previous.ts > downtimeGapMs) {
    intervals.push({ start: Math.max(rangeStart, previous.ts + metricSampleMs), end: rangeEnd });
  } else if (!previous && !sawPointInRange && points.length > 0) {
    intervals.push({ start: rangeStart, end: rangeEnd });
  }

  return intervals.filter(interval => interval.end - interval.start > 0);
}

function renderAvailability(rangeStart, rangeEnd, points, intervals) {
  if (!points.length && !intervals.length) {
    $('chartAvailability').textContent = 'No metric samples for this range yet.';
    return;
  }
  const downtimeMs = intervals.reduce((sum, interval) => sum + Math.max(0, interval.end - interval.start), 0);
  const totalMs = Math.max(1, rangeEnd - rangeStart);
  const uptimePercent = Math.max(0, Math.min(100, 100 - downtimeMs * 100 / totalMs));
  const gapLabel = intervals.length === 1 ? 'gap' : 'gaps';
  $('chartAvailability').textContent =
    'Availability ' + uptimePercent.toFixed(1) + '% | ' +
    formatDowntime(downtimeMs) + ' downtime | ' +
    intervals.length + ' ' + gapLabel + ' | Times shown in ' + browserTimeZoneLabel;
}

function formatDowntime(ms) {
  if (ms < 60 * 1000) return '0m';
  const minutes = Math.round(ms / 60000);
  if (minutes < 60) return minutes + 'm';
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours < 24) return hours + 'h ' + rest + 'm';
  const days = Math.floor(hours / 24);
  const dayHours = hours % 24;
  return days + 'd ' + dayHours + 'h';
}

const downtimeBandsPlugin = {
  id: 'downtimeBands',
  beforeDatasetsDraw(chart) {
    if (!visibleDowntime.length) return;
    const { ctx, chartArea, scales } = chart;
    if (!chartArea || !scales.x) return;
    ctx.save();
    ctx.fillStyle = 'rgba(251,113,133,0.13)';
    for (const interval of visibleDowntime) {
      const left = Math.max(chartArea.left, scales.x.getPixelForValue(interval.start));
      const right = Math.min(chartArea.right, scales.x.getPixelForValue(interval.end));
      if (right - left >= 1) {
        ctx.fillRect(left, chartArea.top, right - left, chartArea.bottom - chartArea.top);
      }
    }
    ctx.restore();
  }
};

function initChartControls() {
  $('chartRanges').addEventListener('click', event => {
    const button = event.target.closest('button[data-hours]');
    if (!button) return;
    chartRangeHours = Number(button.dataset.hours);
    for (const el of $('chartRanges').querySelectorAll('button')) {
      el.classList.toggle('active', el === button);
    }
    renderMetricChart();
  });
}

initChartControls();
refreshAll();
$('commandInput').addEventListener('keydown', handleCommandKey);
startLogPolling();
setInterval(refreshAll, 5000);
</script>
</body>
</html>`))
