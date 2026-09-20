package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"time"

	"gbf-local-cache/internal/cache"
	"gbf-local-cache/internal/cert"
	"gbf-local-cache/internal/config"
	"gbf-local-cache/internal/logging"
	"gbf-local-cache/internal/migration"
	"gbf-local-cache/internal/service"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the small Wails bridge. It deliberately exposes service snapshots
// rather than the cache/proxy implementation, so the UI cannot couple itself
// to backend goroutines.
type App struct {
	mu            sync.RWMutex
	ctx           context.Context
	config        config.Config
	service       *service.Service
	initErr       string
	trayStop      func()
	quitRequested bool
}

type ConnectionTestResult struct {
	OK            bool   `json:"ok"`
	Host          string `json:"host"`
	Status        int    `json:"status"`
	DNSMs         int64  `json:"dns_ms"`
	TCPMs         int64  `json:"tcp_ms"`
	TLSMs         int64  `json:"tls_ms"`
	TTFBMs        int64  `json:"ttfb_ms"`
	DurationMs    int64  `json:"duration_ms"`
	DownloadBytes int64  `json:"download_bytes"`
	ThroughputBPS int64  `json:"throughput_bps"`
	HTTPVersion   string `json:"http_version"`
	Error         string `json:"error,omitempty"`
}

func NewApp() *App {
	app := &App{}

	configPath, err := config.DefaultPath()
	if err != nil {
		app.initErr = err.Error()
		return app
	}

	cfg, err := config.LoadOrCreate(configPath)
	if err != nil {
		app.initErr = err.Error()
		return app
	}

	svc, err := service.New(cfg)
	if err != nil {
		app.initErr = err.Error()
		return app
	}

	app.config = cfg
	app.service = svc
	svc.SetConfigChangedCallback(func(changed config.Config) {
		if err := app.syncPersistedConfig(changed); err != nil {
			svc.Logs().Add(logging.Entry{Category: logging.CategoryError, Message: err.Error()})
		}
	})
	return app
}

func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	initErr := a.initErr
	svc := a.service
	a.mu.Unlock()
	a.startTray(ctx)

	if initErr != "" || svc == nil {
		return
	}
	if err := svc.Start(ctx); err != nil {
		a.mu.Lock()
		a.initErr = err.Error()
		a.mu.Unlock()
	}
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc != nil {
		_ = svc.Stop(ctx)
	}
	a.stopTray()
}

// beforeClose implements the user-selected close behavior. Wails invokes this
// callback for both the window close button and runtime.Quit, so tray exit sets
// quitRequested first to bypass the hide-to-tray preference.
func (a *App) beforeClose(ctx context.Context) bool {
	a.mu.Lock()
	if a.quitRequested {
		a.quitRequested = false
		a.mu.Unlock()
		return false
	}
	behavior := a.config.CloseBehavior
	a.mu.Unlock()

	if behavior == config.CloseBehaviorTray && runtime.GOOS == "windows" {
		wailsruntime.WindowHide(ctx)
		return true
	}
	return false
}

func (a *App) forceQuit() {
	a.mu.Lock()
	a.quitRequested = true
	ctx := a.ctx
	a.mu.Unlock()
	if ctx != nil {
		wailsruntime.Quit(ctx)
	}
}

// Snapshot is called by the overview page. The method returns a serializable
// value and never exposes internal mutable state.
func (a *App) Snapshot() service.Snapshot {
	a.mu.RLock()
	initErr := a.initErr
	svc := a.service
	a.mu.RUnlock()

	if svc == nil {
		return service.Snapshot{State: service.StateError, Error: initErr}
	}
	snapshot := svc.Snapshot()
	_ = a.syncPersistedConfig(svc.Config())
	if initErr != "" {
		snapshot.State = service.StateError
		snapshot.Error = initErr
	}
	return snapshot
}

func (a *App) Start() error {
	a.mu.RLock()
	svc := a.service
	ctx := a.ctx
	initErr := a.initErr
	a.mu.RUnlock()
	if initErr != "" {
		return fmt.Errorf("application initialization failed: %s", initErr)
	}
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return svc.Start(ctx)
}

func (a *App) Stop() error {
	a.mu.RLock()
	svc := a.service
	ctx := a.ctx
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return svc.Stop(ctx)
}

// TestConnection uses the active Origin network manager, so the result
// reflects the currently selected Direct or Clash route.
func (a *App) TestConnection() ConnectionTestResult {
	a.mu.RLock()
	svc := a.service
	ctx := a.ctx
	a.mu.RUnlock()
	if svc == nil {
		return ConnectionTestResult{Error: "service is not initialized"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	testContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result := svc.TestConnection(testContext, "")
	return ConnectionTestResult{
		OK: result.OK, Host: result.Host, Status: result.Status,
		DNSMs: result.DNSMs, TCPMs: result.TCPMs, TLSMs: result.TLSMs,
		TTFBMs: result.TTFBMs, DurationMs: result.DurationMs,
		DownloadBytes: result.DownloadBytes, ThroughputBPS: result.ThroughputBPS,
		HTTPVersion: result.HTTPVersion, Error: result.Error,
	}
}

// OpenCacheFolder opens the active cache directory.
func (a *App) OpenCacheFolder() error {
	a.mu.RLock()
	cacheRoot := a.config.CacheRoot
	svc := a.service
	a.mu.RUnlock()
	if svc != nil {
		cacheRoot = svc.Snapshot().CacheRoot
	}
	if cacheRoot == "" {
		configPath, err := config.DefaultPath()
		if err != nil {
			return err
		}
		cacheRoot = filepath.Join(filepath.Dir(configPath), "cache")
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	return openWindowsFolder(cacheRoot)
}

func (a *App) OpenConfigFolder() error {
	configPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return openWindowsFolder(filepath.Dir(configPath))
}

func (a *App) ClearLogs() error {
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	svc.Logs().Clear()
	return nil
}

func (a *App) ClearCache() error {
	a.mu.RLock()
	svc := a.service
	ctx := a.ctx
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	clearContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return svc.ClearCache(clearContext)
}

func (a *App) InspectCache() (cache.HealthReport, error) {
	a.mu.RLock()
	svc := a.service
	ctx := a.ctx
	a.mu.RUnlock()
	if svc == nil {
		return cache.HealthReport{}, fmt.Errorf("service is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	inspectContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return svc.InspectCache(inspectContext)
}

func (a *App) ChooseCacheRoot() (string, error) {
	a.mu.RLock()
	ctx := a.ctx
	current := a.config.CacheRoot
	a.mu.RUnlock()
	if ctx == nil {
		return "", fmt.Errorf("desktop context is not initialized")
	}
	if err := os.MkdirAll(current, 0o700); err != nil {
		return "", err
	}
	return wailsruntime.OpenDirectoryDialog(ctx, wailsruntime.OpenDialogOptions{
		Title:                "选择 GBF 缓存目录",
		DefaultDirectory:     current,
		CanCreateDirectories: true,
	})
}

func (a *App) ChangeCacheRoot(root string) error {
	a.mu.RLock()
	svc := a.service
	ctx := a.ctx
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	changeContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := svc.ChangeCacheRoot(changeContext, root); err != nil {
		return err
	}
	return a.syncPersistedConfig(svc.Config())
}

func (a *App) PauseMigration() error {
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	return svc.PauseMigration()
}

func (a *App) ResumeMigration() error {
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	return svc.ResumeMigration()
}

func (a *App) CancelMigration() error {
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	return svc.CancelMigration()
}

func (a *App) SetNetworkMode(mode string, protocol string, host string, port int, username string, password string) error {
	if mode != string(config.NetworkModeDirect) && mode != string(config.NetworkModeClash) {
		return fmt.Errorf("unsupported network mode %q", mode)
	}
	a.mu.RLock()
	svc := a.service
	ctx := a.ctx
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	changeContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := svc.SwitchNetwork(changeContext, config.NetworkMode(mode), config.ClashConfig{
		Protocol: protocol, Host: host, Port: port, Username: username, Password: password,
	}); err != nil {
		return err
	}
	return a.syncPersistedConfig(svc.Config())
}

func (a *App) InstallCertificate() error {
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	return svc.InstallCertificate()
}

func (a *App) UninstallCertificate() error {
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	return svc.UninstallCertificate()
}

func (a *App) RegenerateCertificate() error {
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	return svc.RegenerateCertificate()
}

// GetStartupEnabled reports whether this user has enabled Windows startup.
func (a *App) GetStartupEnabled() (bool, error) {
	return startupEnabled()
}

// SetStartupEnabled adds or removes the current executable from the user's
// Windows startup entries. The --background flag makes startup launch into
// the tray without opening the control panel.
func (a *App) SetStartupEnabled(enabled bool) error {
	return setStartupEnabled(enabled)
}

// GetCloseBehavior returns the action used when the main window is closed.
func (a *App) GetCloseBehavior() (string, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.service == nil {
		return "", fmt.Errorf("service is not initialized")
	}
	return string(a.config.CloseBehavior), nil
}

// SetCloseBehavior persists the action used by the main window close button.
func (a *App) SetCloseBehavior(behavior string) error {
	value := config.CloseBehavior(behavior)
	if value != config.CloseBehaviorTray && value != config.CloseBehaviorExit {
		return fmt.Errorf("unsupported close behavior %q", behavior)
	}
	a.mu.RLock()
	svc := a.service
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	cfg := svc.Config()
	cfg.CloseBehavior = value
	if err := svc.SetConfig(cfg); err != nil {
		return err
	}
	return a.syncPersistedConfig(cfg)
}

// SetListenPort changes the loopback proxy port and persists it. When the
// service is running, only the HTTP listener is rebound; cache and TLS state
// stay alive.
func (a *App) SetListenPort(port int) error {
	a.mu.RLock()
	svc := a.service
	ctx := a.ctx
	a.mu.RUnlock()
	if svc == nil {
		return fmt.Errorf("service is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	changeContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := svc.ChangeListenPort(changeContext, port); err != nil {
		return err
	}
	return a.syncPersistedConfig(svc.Config())
}

func (a *App) CertificateStatus() cert.Status {
	snapshot := a.Snapshot()
	return snapshot.Certificate
}

func (a *App) MigrationStatus() migration.Status {
	snapshot := a.Snapshot()
	return snapshot.Migration
}

func (a *App) syncPersistedConfig(cfg config.Config) error {
	a.mu.RLock()
	unchanged := reflect.DeepEqual(a.config, cfg)
	a.mu.RUnlock()
	if unchanged {
		return nil
	}
	path, err := config.DefaultPath()
	if err != nil {
		return fmt.Errorf("configuration applied in memory but could not be persisted: %w", err)
	}
	if err := config.SaveAtomic(path, cfg); err != nil {
		return fmt.Errorf("configuration applied in memory but could not be persisted: %w", err)
	}
	a.mu.Lock()
	a.config = cfg
	a.mu.Unlock()
	return nil
}

func openWindowsFolder(path string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("open folder is only supported on Windows")
	}
	if err := exec.Command("explorer.exe", path).Start(); err != nil {
		return fmt.Errorf("open folder: %w", err)
	}
	return nil
}
