//go:build windows

package platform

import "golang.org/x/sys/windows"

// ReplaceFile atomically replaces destination with source on Windows while
// asking the filesystem to flush the rename through to stable storage.
func ReplaceFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
