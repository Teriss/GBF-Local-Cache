package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gbf-local-cache/internal/host"
	"gbf-local-cache/internal/platform"
)

const CurrentVersion = 1

type NetworkMode string

const (
	NetworkModeDirect NetworkMode = "direct"
	NetworkModeClash  NetworkMode = "clash"
)

type CloseBehavior string

const (
	CloseBehaviorTray CloseBehavior = "tray"
	CloseBehaviorExit CloseBehavior = "exit"
)

type ClashConfig struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type Config struct {
	Version       int           `json:"version"`
	ListenAddress string        `json:"listen_address"`
	CacheRoot     string        `json:"cache_root"`
	RAMCacheMB    int           `json:"ram_cache_mb"`
	RAMObjectMB   int           `json:"ram_object_mb"`
	NetworkMode   NetworkMode   `json:"network_mode"`
	Clash         ClashConfig   `json:"clash"`
	AllowedHosts  []string      `json:"allowed_hosts"`
	CloseBehavior CloseBehavior `json:"close_behavior"`
}

func Default() Config {
	return Config{
		Version:       CurrentVersion,
		ListenAddress: "127.0.0.1:8124",
		CacheRoot:     defaultCacheRoot(),
		RAMCacheMB:    256,
		RAMObjectMB:   8,
		NetworkMode:   NetworkModeDirect,
		CloseBehavior: CloseBehaviorTray,
		Clash: ClashConfig{
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     7897,
		},
		// These are intentionally exact host names. In particular, the public
		// akamaized.net suffix is not treated as a wildcard.
		AllowedHosts: []string{
			"prd-game-a-granbluefantasy.akamaized.net",
		},
	}
}

func DefaultPath() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve config directory: %w", err)
		}
	}
	return filepath.Join(base, "GBFLocalCache", "config.json"), nil
}

func LoadOrCreate(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := Default()
		if err := SaveAtomic(path, cfg); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	cfg := Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if cfg.Version == 0 {
		cfg.Version = CurrentVersion
	}
	if cfg.CacheRoot == "" {
		cfg.CacheRoot = defaultCacheRoot()
	}
	if cfg.RAMCacheMB == 0 {
		cfg.RAMCacheMB = 256
	}
	if cfg.RAMObjectMB == 0 {
		cfg.RAMObjectMB = 8
	}
	if cfg.CloseBehavior == "" {
		cfg.CloseBehavior = CloseBehaviorTray
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func SaveAtomic(path string, cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restrict config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}

	if err := platform.ReplaceFile(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func Validate(cfg Config) error {
	if cfg.Version != CurrentVersion {
		return fmt.Errorf("unsupported config version %d", cfg.Version)
	}
	if err := validateLoopbackListenAddress(cfg.ListenAddress); err != nil {
		return err
	}
	if cfg.NetworkMode != NetworkModeDirect && cfg.NetworkMode != NetworkModeClash {
		return fmt.Errorf("unsupported network mode %q", cfg.NetworkMode)
	}
	if cfg.CloseBehavior != CloseBehaviorTray && cfg.CloseBehavior != CloseBehaviorExit {
		return fmt.Errorf("unsupported close behavior %q", cfg.CloseBehavior)
	}
	if cfg.CacheRoot == "" {
		return fmt.Errorf("cache root must not be empty")
	}
	if cfg.RAMCacheMB != 64 && cfg.RAMCacheMB != 128 && cfg.RAMCacheMB != 256 && cfg.RAMCacheMB != 512 && cfg.RAMCacheMB != 1024 {
		return fmt.Errorf("RAM cache size must be 64, 128, 256, 512, or 1024 MB")
	}
	if cfg.RAMObjectMB < 1 || cfg.RAMObjectMB > cfg.RAMCacheMB {
		return fmt.Errorf("RAM object size is invalid")
	}
	if cfg.NetworkMode == NetworkModeClash {
		protocol := strings.ToLower(strings.TrimSpace(cfg.Clash.Protocol))
		if protocol != "http" && protocol != "socks5" {
			return fmt.Errorf("unsupported Clash protocol %q", cfg.Clash.Protocol)
		}
		if net.ParseIP(cfg.Clash.Host) == nil && strings.TrimSpace(cfg.Clash.Host) == "" {
			return fmt.Errorf("Clash host must not be empty")
		}
		if cfg.Clash.Port < 1 || cfg.Clash.Port > 65535 {
			return fmt.Errorf("Clash port must be between 1 and 65535")
		}
	}
	if len(cfg.AllowedHosts) == 0 {
		return fmt.Errorf("at least one allowed CDN host is required")
	}
	for _, allowedHost := range cfg.AllowedHosts {
		if _, ok := host.Normalize(allowedHost); !ok {
			return fmt.Errorf("invalid allowed CDN host %q", allowedHost)
		}
	}
	return nil
}

func defaultCacheRoot() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base, _ = os.UserConfigDir()
	}
	if base == "" {
		return filepath.Join(".", "GBFLocalCache", "cache")
	}
	return filepath.Join(base, "GBFLocalCache", "cache")
}

func validateLoopbackListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	if host == "localhost" {
		return fmt.Errorf("listen address must be an explicit loopback IP, not localhost")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q is not loopback-only", address)
	}
	if port == "" {
		return fmt.Errorf("listen port is empty")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("listen port %q must be between 1 and 65535", port)
	}
	return nil
}
