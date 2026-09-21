//go:build openbsd

package process

import (
	"unsafe"

	ps "github.com/shirou/gopsutil/process"
)

func bsdKernelProcesses(pid int) ([]platformProcess, error) {
	var layout ps.KinfoProc
	size := int(unsafe.Sizeof(layout))
	mib := []int32{1, 66, 0, 0, int32(size), 0} // CTL_KERN, KERN_PROC, KERN_PROC_ALL
	if pid != 0 {
		mib[2], mib[3] = 1, int32(pid) // KERN_PROC_PID
	}
	data, err := bsdSysctl(mib, size, true)
	if err != nil {
		return nil, err
	}
	result := make([]platformProcess, 0, len(data)/size)
	for offset := 0; offset < len(data); offset += size {
		record := data[offset : offset+size]
		stat := record[unsafe.Offsetof(layout.Stat)]
		result = append(result, platformProcess{
			pid:  bsdUint32(record, unsafe.Offsetof(layout.Pid)),
			ppid: bsdUint32(record, unsafe.Offsetof(layout.Ppid)),
			pgid: bsdUint32(record, unsafe.Offsetof(layout.X_pgid)),
			identity: bsdUint64(record, unsafe.Offsetof(layout.Ustart_sec))*1_000_000 +
				uint64(bsdUint32(record, unsafe.Offsetof(layout.Ustart_usec))),
			name:   bsdName(record, unsafe.Offsetof(layout.Comm), unsafe.Sizeof(layout.Comm)),
			zombie: stat == 5 || stat == 6,
		})
	}
	return result, nil
}
