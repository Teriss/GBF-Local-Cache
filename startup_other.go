//go:build !windows

package main

import "errors"

func startupEnabled() (bool, error) {
	return false, errors.New("startup settings are only supported on Windows")
}

func setStartupEnabled(bool) error {
	return errors.New("startup settings are only supported on Windows")
}
