package main

//#cgo LDFLAGS: -lsigar
//#include <sigar.h>
//#include <sigar_control_group.h>
import "C"
import (
	"errors"
	"fmt"

	log "github.com/couchbase/clog"
)

var (
	sigarCgroupSupported uint8 = 1
)

type systemStats struct {
	handle *C.sigar_t
	pid    C.sigar_pid_t
}

func NewSystemStats() (*systemStats, error) {

	var handle *C.sigar_t

	if err := C.sigar_open(&handle); err != C.SIGAR_OK {
		return nil, errors.New(fmt.Sprintf("Fail to open sigar.  Error code = %v", err))
	}

	s := &systemStats{}
	s.handle = handle
	s.pid = C.sigar_pid_get(handle)

	return s, nil
}

func (s *systemStats) Close() {
	C.sigar_close(s.handle)
}

// Returns total memory based on cgroup limits, if possible.
func getMemoryLimit() (uint64, error) {
	stats, err := NewSystemStats()
	if err != nil {
		log.Printf("error getting new stats: %+v", err)
		return 0, err
	}
	defer stats.Close()

	memTotal, err := stats.SystemTotalMem()
	if err != nil {
		log.Printf("error getting total mem: %+v", err)
		return 0, err
	}

	cgroupInfo := stats.GetControlGroupInfo()
	if cgroupInfo.Supported == sigarCgroupSupported {
		log.Printf("init_mem: cgroups are supported")
		cGroupTotal := cgroupInfo.MemoryMax
		// cGroupTotal is with-in valid system limits
		if cGroupTotal > 0 && cGroupTotal <= memTotal {
			return cGroupTotal, nil
		}
	}

	log.Printf("init_mem: memory total is: %d", memTotal)
	return memTotal, nil
}

// return the hosts-level memory limit in bytes.
func (s *systemStats) SystemTotalMem() (uint64, error) {
	var mem C.sigar_mem_t
	if err := C.sigar_mem_get(s.handle, &mem); err != C.SIGAR_OK {
		return uint64(0), errors.New(fmt.Sprintf("Fail to get total memory.  Err=%v", C.sigar_strerror(s.handle, err)))
	}
	return uint64(mem.total), nil
}

// Subset of the cgroup info statistics relevant to FTS.
type sigarControlGroupInfo struct {
	Supported uint8 // "1" if cgroup info is supprted, "0" otherwise
	Version   uint8 // "1" for cgroup v1, "2" for cgroup v2

	// Maximum memory available in the group. Derived from memory.max
	MemoryMax uint64
}

// GetControlGroupInfo returns the fields of C.sigar_control_group_info_t FTS uses. These reflect
// Linux control group settings, which are used by Kubernetes to set pod memory and CPU limits.
func (h *systemStats) GetControlGroupInfo() *sigarControlGroupInfo {
	var info C.sigar_control_group_info_t
	C.sigar_get_control_group_info(&info)

	return &sigarControlGroupInfo{
		Supported: uint8(info.supported),
		Version:   uint8(info.version),
		MemoryMax: uint64(info.memory_max),
	}
}
