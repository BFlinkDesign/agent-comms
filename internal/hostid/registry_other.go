//go:build !windows

package hostid

import "errors"

// machineGUID exists only on Windows; elsewhere the reg.exe path, which tests
// drive through Options.Runner, is the only one.
func machineGUID() (string, error) {
	return "", errors.New("hostid: the Windows registry is not available on this platform")
}
