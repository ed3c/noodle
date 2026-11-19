//go:build darwin && cgo

package admission

/*
#include <mach/mach.h>

static kern_return_t noodle_cpu_ticks(host_cpu_load_info_t info, mach_msg_type_number_t *count) {
    host_t host = mach_host_self();
    kern_return_t result = host_statistics(host, HOST_CPU_LOAD_INFO, (host_info_t)info, count);
    mach_port_deallocate(mach_task_self(), host);
    return result;
}
*/
import "C"

import "fmt"

func readDarwinCPUTicks() (*CPUCounters, error) {
	var info C.host_cpu_load_info_data_t
	count := C.mach_msg_type_number_t(C.HOST_CPU_LOAD_INFO_COUNT)
	if code := C.noodle_cpu_ticks(&info, &count); code != C.KERN_SUCCESS {
		return nil, fmt.Errorf("read Mach CPU ticks: host_statistics returned %d", code)
	}
	if count != C.HOST_CPU_LOAD_INFO_COUNT {
		return nil, fmt.Errorf("read Mach CPU ticks: counter count %d differs from %d", count, C.HOST_CPU_LOAD_INFO_COUNT)
	}
	// HOST_CPU_LOAD_INFO sums scheduler ticks across processors; it is not a core quota.
	return &CPUCounters{
		User: uint64(info.cpu_ticks[C.CPU_STATE_USER]), System: uint64(info.cpu_ticks[C.CPU_STATE_SYSTEM]),
		Nice: uint64(info.cpu_ticks[C.CPU_STATE_NICE]), Idle: uint64(info.cpu_ticks[C.CPU_STATE_IDLE]),
	}, nil
}
