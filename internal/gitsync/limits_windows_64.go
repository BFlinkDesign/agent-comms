//go:build windows && (amd64 || arm64)

package gitsync

import "unsafe"

// basicLimitInformation mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION on 64-bit
// Windows.
type basicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// The C layout: 64 bytes, then IO_COUNTERS at 64, 144 in all. A mismatch does
// not compile.
var (
	_ [0]struct{} = [unsafe.Sizeof(basicLimitInformation{}) - 64]struct{}{}
	_ [0]struct{} = [unsafe.Offsetof(extendedLimitInformation{}.IoInfo) - 64]struct{}{}
	_ [0]struct{} = [unsafe.Sizeof(extendedLimitInformation{}) - 144]struct{}{}
)
