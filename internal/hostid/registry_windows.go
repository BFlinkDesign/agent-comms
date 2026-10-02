//go:build windows

package hostid

import (
	"errors"
	"syscall"
	"unsafe"
)

// machineGUID reads HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid in this
// process, from the 64-bit registry view whatever this binary's architecture.
// Starting reg.exe for it cost a child process on every fleetd command, which an
// endpoint agent may scan, delay or flag as discovery, and a 3-second deadline
// it could miss: missing it degraded that one record to a hostname-only
// identity, filed under a different host.
func machineGUID() (string, error) {
	subkey, err := syscall.UTF16PtrFromString(`SOFTWARE\Microsoft\Cryptography`)
	if err != nil {
		return "", err
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, subkey, 0,
		syscall.KEY_QUERY_VALUE|syscall.KEY_WOW64_64KEY, &key); err != nil {
		return "", err
	}
	defer syscall.RegCloseKey(key)
	name, err := syscall.UTF16PtrFromString("MachineGuid")
	if err != nil {
		return "", err
	}
	var kind, size uint32
	if err := syscall.RegQueryValueEx(key, name, nil, &kind, nil, &size); err != nil {
		return "", err
	}
	if kind != syscall.REG_SZ || size < 2 || size > 1024 {
		return "", errors.New("hostid: MachineGuid is not a short REG_SZ value")
	}
	buf := make([]uint16, size/2)
	if err := syscall.RegQueryValueEx(key, name, nil, &kind, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil {
		return "", err
	}
	return syscall.UTF16ToString(buf), nil
}
