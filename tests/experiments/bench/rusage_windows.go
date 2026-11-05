//go:build windows

package main

import (
	"math"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var psapi = windows.NewLazySystemDLL("psapi.dll")
var getProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func procUsage(pid int) (time.Duration, int64, bool) {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return 0, 0, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, 0, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, 0, false
	}
	counters := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	ret, _, _ := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if ret == 0 {
		return 0, 0, false
	}
	cpu100ns := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
	cpu100ns += uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
	return time.Duration(cpu100ns) * 100, int64(counters.WorkingSetSize / 1024), true
}
