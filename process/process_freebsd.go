//go:build freebsd

package process

import (
	"unsafe"

	ps "github.com/shirou/gopsutil/process"
)

func bsdKernelProcesses(pid int) ([]platformProcess, error) {
	var layout ps.KinfoProc
	size := int(unsafe.Sizeof(layout))
	mib := []int32{1, 14, 8, 0} // CTL_KERN, KERN_PROC, KERN_PROC_PROC
	if pid != 0 {
		mib[2], mib[3] = 1, int32(pid) // KERN_PROC_PID
	}
	data, err := bsdSysctl(mib, size, false)
	if err != nil {
		return nil, err
	}
	result := make([]platformProcess, 0, len(data)/size)
	for offset := 0; offset < len(data); offset += size {
		record := data[offset : offset+size]
		start := unsafe.Offsetof(layout.Start)
		result = append(result, platformProcess{
			pid:      bsdUint32(record, unsafe.Offsetof(layout.Pid)),
			ppid:     bsdUint32(record, unsafe.Offsetof(layout.Ppid)),
			pgid:     bsdUint32(record, unsafe.Offsetof(layout.Pgid)),
			identity: bsdUint64(record, start)*1_000_000 + bsdUint64(record, start+8),
			name:     bsdName(record, unsafe.Offsetof(layout.Comm), unsafe.Sizeof(layout.Comm)),
			zombie:   record[unsafe.Offsetof(layout.Stat)] == 5,
		})
	}
	return result, nil
}
