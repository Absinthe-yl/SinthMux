//go:build darwin

package main

/*
#include <libproc.h>
#include <sys/resource.h>
#include <mach/mach_time.h>

// usage reads CPU time (ns) and resident memory of a process. It works where
// ps and top are not allowed (sandboxed shells).
static int usage(int pid, uint64_t *cpu, uint64_t *rss) {
	struct rusage_info_v2 ri;
	int r = proc_pid_rusage(pid, RUSAGE_INFO_V2, (rusage_info_t *)&ri);
	if (r != 0) return r;
	mach_timebase_info_data_t tb;
	mach_timebase_info(&tb);
	*cpu = (ri.ri_user_time + ri.ri_system_time) * tb.numer / tb.denom;
	*rss = ri.ri_resident_size;
	return 0;
}
*/
import "C"

import "time"

// procUsage returns CPU time and resident set size in KiB.
func procUsage(pid int) (time.Duration, int64, bool) {
	var cpu, rss C.uint64_t
	if C.usage(C.int(pid), &cpu, &rss) != 0 {
		return 0, 0, false
	}
	return time.Duration(cpu), int64(rss) / 1024, true
}
