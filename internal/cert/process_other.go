//go:build !windows

package cert

import "os/exec"

func runCertificateCommand(name string, arguments ...string) ([]byte, error) {
	return exec.Command(name, arguments...).CombinedOutput()
}
