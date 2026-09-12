//go:build linux

package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func snapshotPlatformProcesses() (map[int]platformProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	ps := make(map[int]platformProcess, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := readLinuxProcess(pid)
		if err == nil {
			ps[pid] = p
		}
	}
	return ps, nil
}
func lookupPlatformProcess(pid int) (platformProcess, error) { return readLinuxProcess(pid) }
func readLinuxProcess(pid int) (platformProcess, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return platformProcess{}, err
	}
	text := string(data)
	left, right := strings.IndexByte(text, '('), strings.LastIndex(text, ") ")
	if left < 0 || right <= left {
		return platformProcess{}, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(text[right+2:])
	if len(fields) <= 19 {
		return platformProcess{}, fmt.Errorf("short /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return platformProcess{}, err
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return platformProcess{}, err
	}
	identity, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return platformProcess{}, err
	}
	return platformProcess{pid: pid, ppid: ppid, pgid: pgid, identity: identity, name: text[left+1 : right], zombie: fields[0] == "Z"}, nil
}
