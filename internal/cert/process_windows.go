//go:build windows

package cert

import (
	"os/exec"
	"syscall"
)

func runCertificateCommand(name string, arguments ...string) ([]byte, error) {
	command := exec.Command(name, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return command.CombinedOutput()
}
