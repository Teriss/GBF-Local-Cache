//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const startupRunPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const startupRunName = "GBFLocalCache"

func startupCommand() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return `"` + executable + `" --background`, nil
}

func startupEnabled() (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, startupRunPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer key.Close()
	value, _, err := key.GetStringValue(startupRunName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return false, err
	}
	return strings.Contains(strings.ToLower(value), strings.ToLower(executable)), nil
}

func setStartupEnabled(enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, startupRunPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("open Windows startup entries: %w", err)
	}
	defer key.Close()
	if !enabled {
		if err := key.DeleteValue(startupRunName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("remove Windows startup entry: %w", err)
		}
		return nil
	}
	command, err := startupCommand()
	if err != nil {
		return err
	}
	if err := key.SetStringValue(startupRunName, command); err != nil {
		return fmt.Errorf("write Windows startup entry: %w", err)
	}
	return nil
}
