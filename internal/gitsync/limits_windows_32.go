//go:build windows && (386 || arm)

package gitsync

import "unsafe"

// basicLimitInformation mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION on 32-bit
// Windows. Go aligns int64 to 4 bytes there and C to 8, so the trailing pad
// makes up the difference.
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
	_                       uint32
}

// The C layout: 48 bytes, then IO_COUNTERS at 48, 112 in all. A mismatch does
// not compile.
var (
	_ [0]struct{} = [unsafe.Sizeof(basicLimitInformation{}) - 48]struct{}{}
	_ [0]struct{} = [unsafe.Offsetof(extendedLimitInformation{}.IoInfo) - 48]struct{}{}
	_ [0]struct{} = [unsafe.Sizeof(extendedLimitInformation{}) - 112]struct{}{}
)
