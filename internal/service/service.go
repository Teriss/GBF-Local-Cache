package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gbf-local-cache/internal/cache"
	"gbf-local-cache/internal/cert"
	"gbf-local-cache/internal/config"
	"gbf-local-cache/internal/host"
	"gbf-local-cache/internal/logging"
	"gbf-local-cache/internal/migration"
	"gbf-local-cache/internal/network"
	"gbf-local-cache/internal/proxy"
	"gbf-local-cache/internal/stats"
)

type State string

const (
	StateStopped   State = "stopped"
	StateStarting  State = "starting"
	StateRunning   State = "running"
	StateStopping  State = "stopping"
	StateError     State = "error"
	StateMigrating State = "migrating"
)

type Snapshot struct {
	State         State              `json:"state"`
	ListenAddress string             `json:"listen_address"`
	NetworkMode   config.NetworkMode `json:"network_mode"`
	CacheRoot     string             `json:"cache_root"`
	RAMUsedBytes  int64              `json:"ram_used_bytes"`
	RAMMaxBytes   int64              `json:"ram_max_bytes"`
	DiskBytes     int64              `json:"disk_bytes"`
	Stats         stats.Counters     `json:"stats"`
	Certificate   cert.Status        `json:"certificate"`
	Migration     migration.Status   `json:"migration"`
	Logs          []logging.Entry    `json:"logs"`
	Error         string             `json:"error,omitempty"`
}

type Service struct {
	mu        sync.RWMutex
	state     State
	lastError string
	config    config.Config
	listener  *proxy.Listener
	origin    *network.Manager
	cache     *cache.Manager
	ca        *cert.Manager
	migration *migration.Manager
	stats     *stats.Stats
	logs      *logging.Ring
}

func New(cfg config.Config) (*Service, error) {
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	return &Service{
		state:  StateStopped,
		config: cfg,
		stats:  &stats.Stats{},
		logs:   logging.NewRing(5000),
	}, nil
}

func (s *Service) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.state == StateRunning {
		s.mu.Unlock()
		return nil
	}
	if s.state == StateStarting || s.state == StateStopping {
		s.mu.Unlock()
		return errors.New("service is busy")
	}
	s.state = StateStarting
	s.lastError = ""
	cfg := s.config
	activeMigration := s.migration
	if activeMigration == nil || (activeMigration.Status().State != migration.StateRunning && activeMigration.Status().State != migration.StatePaused) {
		activeMigration = nil
	}
	s.mu.Unlock()

	if err := os.MkdirAll(cfg.CacheRoot, 0o700); err != nil {
		return s.startFailure(fmt.Errorf("create cache root: %w", err))
	}
	origin, err := network.NewManager(cfg)
	if err != nil {
		return s.startFailure(fmt.Errorf("create origin network: %w", err))
	}
	certDirectory, err := cert.DefaultDirectory()
	if err != nil {
		origin.CloseIdleConnections()
		return s.startFailure(fmt.Errorf("resolve certificate directory: %w", err))
	}
	ca := cert.New(certDirectory)
	if err := ca.Ensure(); err != nil {
		origin.CloseIdleConnections()
		return s.startFailure(fmt.Errorf("initialize Root CA: %w", err))
	}
	managerConfig := cache.ManagerConfig{
		RAMBytes:       int64(cfg.RAMCacheMB) << 20,
		RAMObjectBytes: int64(cfg.RAMObjectMB) << 20,
		Origin:         origin,
		Stats:          s.stats,
		Logs:           s.logs,
	}
	if activeMigration != nil && activeMigration.Roots() != nil {
		managerConfig.Roots = activeMigration.Roots()
	} else {
		managerConfig.Root = cfg.CacheRoot
	}
	manager, err := cache.NewManager(managerConfig)
	if err != nil {
		origin.CloseIdleConnections()
		return s.startFailure(fmt.Errorf("create cache manager: %w", err))
	}

	matcher := host.New(cfg.AllowedHosts, nil)
	core, err := proxy.NewCachedServer(matcher, origin, manager, ca, s.stats, s.logs)
	if err != nil {
		origin.CloseIdleConnections()
		return s.startFailure(fmt.Errorf("create proxy: %w", err))
	}
	listener, err := proxy.StartListener(cfg.ListenAddress, core)
	if err != nil {
		origin.CloseIdleConnections()
		return s.startFailure(fmt.Errorf("start proxy: %w", err))
	}

	migrator := activeMigration
	if migrator == nil {
		migrator = migration.New(manager.Roots(), s.logs)
	}
	s.mu.Lock()
	s.origin = origin
	s.cache = manager
	s.ca = ca
	s.migration = migrator
	s.listener = listener
	s.state = StateRunning
	s.lastError = ""
	s.mu.Unlock()
	s.logs.Add(logging.Entry{Category: logging.CategoryNetwork, Message: "service started on " + listener.Address()})
	resumeCallback := func(status migration.Status) {
		s.mu.Lock()
		if status.State == migration.StateCompleted {
			s.config.CacheRoot = status.NewRoot
		} else if status.State == migration.StateCanceled || status.State == migration.StateError {
			s.config.CacheRoot = status.OldRoot
		}
		s.mu.Unlock()
	}
	if activeMigration != nil {
		return nil
	}
	if recovered, recoverErr := migrator.Recover(ctx, cfg.CacheRoot, resumeCallback); recoverErr != nil {
		s.logs.Add(logging.Entry{Category: logging.CategoryMigration, Message: "migration recovery skipped: " + recoverErr.Error()})
	} else if recovered {
		s.logs.Add(logging.Entry{Category: logging.CategoryMigration, Message: "migration recovery started"})
	}
	return nil
}

func (s *Service) startFailure(err error) error {
	s.mu.Lock()
	s.state = StateError
	s.lastError = err.Error()
	s.mu.Unlock()
	return err
}

func (s *Service) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}

	s.mu.Lock()
	if s.state == StateStopped {
		activeMigration := s.migration != nil
		if activeMigration {
			status := s.migration.Status()
			activeMigration = status.State == migration.StateRunning || status.State == migration.StatePaused
		}
		if !activeMigration {
			s.mu.Unlock()
			return nil
		}
	}
	s.state = StateStopping
	listener := s.listener
	origin := s.origin
	manager := s.cache
	migrator := s.migration
	s.mu.Unlock()

	var stopErr error
	if listener != nil {
		stopErr = listener.Shutdown(ctx)
	}
	if migrator != nil {
		if err := migrator.Close(ctx); stopErr == nil && err != nil {
			stopErr = err
		}
	}
	if manager != nil {
		if err := manager.Wait(ctx); stopErr == nil && err != nil {
			stopErr = err
		}
	}
	if origin != nil {
		origin.CloseIdleConnections()
	}

	s.mu.Lock()
	s.listener = nil
	s.origin = nil
	s.cache = manager
	s.migration = migrator
	if stopErr != nil {
		s.state = StateError
		s.lastError = stopErr.Error()
	} else {
		s.state = StateStopped
		s.lastError = ""
	}
	s.mu.Unlock()
	return stopErr
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	state := s.state
	lastError := s.lastError
	cfg := s.config
	listener := s.listener
	manager := s.cache
	ca := s.ca
	migrator := s.migration
	s.mu.RUnlock()

	listenAddress := cfg.ListenAddress
	if listener != nil && listener.Address() != "" {
		listenAddress = listener.Address()
	}
	cacheRoot := cfg.CacheRoot
	var ramUsed, ramMax, diskBytes int64
	if manager != nil {
		primary, _ := manager.Roots().Roots()
		if primary != "" {
			cacheRoot = primary
		}
		ramUsed = manager.RAMUsedBytes()
		ramMax = manager.RAMMaxBytes()
		diskBytes = manager.DiskSize()
	}
	migrationStatus := migration.Status{State: migration.StateIdle}
	if migrator != nil {
		migrationStatus = migrator.Status()
		if manager == nil && (migrationStatus.State == migration.StateRunning || migrationStatus.State == migration.StatePaused) && migrator.Roots() != nil {
			if primary, _ := migrator.Roots().Roots(); primary != "" {
				cacheRoot = primary
			}
		}
		if migrationStatus.State == migration.StateRunning || migrationStatus.State == migration.StatePaused {
			state = StateMigrating
		}
	}
	return Snapshot{
		State:         state,
		ListenAddress: listenAddress,
		NetworkMode:   cfg.NetworkMode,
		CacheRoot:     cacheRoot,
		RAMUsedBytes:  ramUsed,
		RAMMaxBytes:   ramMax,
		DiskBytes:     diskBytes,
		Stats:         s.stats.Snapshot(),
		Certificate:   certificateStatus(ca),
		Migration:     migrationStatus,
		Logs:          s.logs.Snapshot(),
		Error:         lastError,
	}
}

func certificateStatus(manager *cert.Manager) cert.Status {
	if manager == nil {
		return cert.Status{}
	}
	return manager.Status()
}

func (s *Service) Stats() *stats.Stats {
	return s.stats
}

func (s *Service) Logs() *logging.Ring {
	return s.logs
}

func (s *Service) Config() config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.config)
}

func (s *Service) SetConfig(cfg config.Config) error {
	if err := config.Validate(cfg); err != nil {
		return err
	}
	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()
	return nil
}

func (s *Service) TestConnection(ctx context.Context, targetHost string) network.TestResult {
	s.mu.RLock()
	origin := s.origin
	s.mu.RUnlock()
	if origin == nil {
		return network.TestResult{Host: targetHost, Error: "service is not running"}
	}
	if strings.TrimSpace(targetHost) == "" {
		cfg := s.Config()
		if len(cfg.AllowedHosts) > 0 {
			targetHost = cfg.AllowedHosts[0]
		}
	}
	if _, ok := host.Normalize(targetHost); !ok {
		return network.TestResult{Host: targetHost, Error: "测试目标不在合法 CDN 主机格式内"}
	}
	return origin.Test(ctx, targetHost)
}

func (s *Service) SwitchNetwork(ctx context.Context, mode config.NetworkMode, clash config.ClashConfig) error {
	s.mu.RLock()
	cfg := s.config
	origin := s.origin
	running := s.state == StateRunning || s.state == StateMigrating
	s.mu.RUnlock()
	cfg.NetworkMode = mode
	cfg.Clash = clash
	if err := config.Validate(cfg); err != nil {
		return err
	}
	if running && origin != nil {
		testHost := ""
		if len(cfg.AllowedHosts) > 0 {
			testHost = cfg.AllowedHosts[0]
		}
		if err := origin.Switch(ctx, cfg, testHost); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.config.NetworkMode = cfg.NetworkMode
	s.config.Clash = cfg.Clash
	s.mu.Unlock()
	s.logs.Add(logging.Entry{Category: logging.CategoryNetwork, Message: "network mode changed to " + string(mode)})
	return nil
}

func (s *Service) ClearCache(ctx context.Context) error {
	s.mu.RLock()
	manager := s.cache
	s.mu.RUnlock()
	if manager == nil {
		return errors.New("cache manager is not initialized")
	}
	if err := manager.Clear(ctx); err != nil {
		return err
	}
	s.logs.Add(logging.Entry{Category: logging.CategoryStore, Message: "cache cleared"})
	return nil
}

func (s *Service) InspectCache(ctx context.Context) (cache.HealthReport, error) {
	s.mu.RLock()
	manager := s.cache
	s.mu.RUnlock()
	if manager == nil {
		return cache.HealthReport{}, errors.New("cache manager is not initialized")
	}
	return manager.Inspect(ctx)
}

func (s *Service) ChangeCacheRoot(ctx context.Context, newRoot string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	newRoot = strings.TrimSpace(newRoot)
	if newRoot == "" {
		return errors.New("new cache root is empty")
	}
	newRoot, err := filepath.Abs(filepath.Clean(newRoot))
	if err != nil {
		return err
	}
	s.mu.RLock()
	oldRoot := s.config.CacheRoot
	manager := s.cache
	migrator := s.migration
	s.mu.RUnlock()
	oldRoot, _ = filepath.Abs(filepath.Clean(oldRoot))
	if strings.EqualFold(oldRoot, newRoot) {
		return nil
	}
	if migrator != nil {
		status := migrator.Status()
		if status.State == migration.StateRunning || status.State == migration.StatePaused {
			return errors.New("cache migration is already running")
		}
	}
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return fmt.Errorf("create current cache root: %w", err)
	}
	if manager != nil {
		if err := manager.Wait(ctx); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(newRoot), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(newRoot); errors.Is(err, os.ErrNotExist) && filepath.VolumeName(oldRoot) == filepath.VolumeName(newRoot) {
		if err := os.Rename(oldRoot, newRoot); err == nil {
			roots := cache.NewRootSet(oldRoot)
			if manager != nil {
				roots = manager.Roots()
			}
			roots.BeginMigration(newRoot, "")
			roots.FinishMigration()
			s.setCacheRoot(newRoot)
			s.logs.Add(logging.Entry{Category: logging.CategoryMigration, Message: "cache root renamed to " + newRoot})
			return nil
		}
	}
	if migrator == nil {
		roots := cache.NewRootSet(oldRoot)
		if manager != nil {
			roots = manager.Roots()
		}
		migrator = migration.New(roots, s.logs)
		s.mu.Lock()
		if s.migration == nil {
			s.migration = migrator
		} else {
			migrator = s.migration
		}
		s.mu.Unlock()
	}
	callback := func(status migration.Status) {
		s.mu.Lock()
		if status.State == migration.StateCompleted {
			s.config.CacheRoot = newRoot
		} else if status.State == migration.StateCanceled || status.State == migration.StateError {
			s.config.CacheRoot = oldRoot
		}
		s.mu.Unlock()
	}
	if err := migrator.Start(ctx, oldRoot, newRoot, callback); err != nil {
		return err
	}
	s.logs.Add(logging.Entry{Category: logging.CategoryMigration, Message: "cache migration started while service is stopped or running"})
	return nil
}

func (s *Service) setCacheRoot(root string) {
	s.mu.Lock()
	s.config.CacheRoot = root
	s.mu.Unlock()
}

func (s *Service) PauseMigration() error {
	s.mu.RLock()
	migrator := s.migration
	s.mu.RUnlock()
	if migrator == nil {
		return errors.New("migration manager is not initialized")
	}
	return migrator.Pause()
}

func (s *Service) ResumeMigration() error {
	s.mu.RLock()
	migrator := s.migration
	s.mu.RUnlock()
	if migrator == nil {
		return errors.New("migration manager is not initialized")
	}
	return migrator.Resume()
}

func (s *Service) CancelMigration() error {
	s.mu.RLock()
	migrator := s.migration
	s.mu.RUnlock()
	if migrator == nil {
		return errors.New("migration manager is not initialized")
	}
	return migrator.Cancel()
}

func (s *Service) InstallCertificate() error {
	s.mu.RLock()
	ca := s.ca
	s.mu.RUnlock()
	if ca == nil {
		return errors.New("certificate manager is not initialized")
	}
	if err := ca.Install(); err != nil {
		return err
	}
	s.logs.Add(logging.Entry{Category: logging.CategoryCert, Message: "Root CA installed"})
	return nil
}

func (s *Service) UninstallCertificate() error {
	s.mu.RLock()
	ca := s.ca
	s.mu.RUnlock()
	if ca == nil {
		return errors.New("certificate manager is not initialized")
	}
	if err := ca.Uninstall(); err != nil {
		return err
	}
	s.logs.Add(logging.Entry{Category: logging.CategoryCert, Message: "Root CA uninstalled"})
	return nil
}

func (s *Service) RegenerateCertificate() error {
	s.mu.RLock()
	ca := s.ca
	s.mu.RUnlock()
	if ca == nil {
		return errors.New("certificate manager is not initialized")
	}
	if err := ca.Regenerate(); err != nil {
		return err
	}
	s.logs.Add(logging.Entry{Category: logging.CategoryCert, Message: "Root CA regenerated"})
	return nil
}

func cloneConfig(cfg config.Config) config.Config {
	cfg.AllowedHosts = append([]string(nil), cfg.AllowedHosts...)
	return cfg
}
