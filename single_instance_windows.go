//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

const singleInstanceName = `Global\GBFLocalCache.SingleInstance`

// acquireSingleInstance keeps the desktop application from starting a second
// service process that would compete for the same local proxy port.
func acquireSingleInstance() (release func(), alreadyRunning bool, err error) {
	name, err := windows.UTF16PtrFromString(singleInstanceName)
	if err != nil {
		return nil, false, fmt.Errorf("create single-instance name: %w", err)
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if handle == 0 {
		return nil, false, fmt.Errorf("create single-instance mutex: %w", err)
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		_ = windows.CloseHandle(handle)
		return func() {}, true, nil
	}
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, false, fmt.Errorf("create single-instance mutex: %w", err)
	}
	return func() { _ = windows.CloseHandle(handle) }, false, nil
}
