//go:build !windows

package platform

import "os"

// ReplaceFile uses the platform's same-filesystem rename semantics.
func ReplaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
