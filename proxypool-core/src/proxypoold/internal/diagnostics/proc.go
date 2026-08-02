package diagnostics

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const maxManagedProcessEntries = 128

var managedProcessNames = map[string]struct{}{
	"proxypoold": {}, "xl2tpd": {}, "pppd": {}, "slp-client": {},
}

type managedProcessMetadata struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	Name    string `json:"name"`
	State   string `json:"state,omitempty"`
	RSSKiB  uint64 `json:"rss_kib"`
	Threads int    `json:"threads"`
	FDCount int    `json:"fd_count"`
}

type managedProcessDocument struct {
	Processes []managedProcessMetadata `json:"processes"`
	ErrorCode string                   `json:"error_code,omitempty"`
}

// ReadManagedProcessMetadata reads no argv, environment, maps, or open-file
// targets. Only fixed /proc status counters for ProxyPool-related processes
// are included, so unrelated process credentials cannot enter a bundle.
func ReadManagedProcessMetadata(root string) []byte {
	document := managedProcessDocument{Processes: []managedProcessMetadata{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		document.ErrorCode = "unavailable"
		encoded, _ := json.Marshal(document)
		return encoded
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid < 1 || !entry.IsDir() {
			continue
		}
		processRoot := filepath.Join(root, entry.Name())
		comm, err := readBoundedProcFile(filepath.Join(processRoot, "comm"), 256)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if _, allowed := managedProcessNames[name]; !allowed {
			continue
		}
		metadata := managedProcessMetadata{PID: pid, Name: name, FDCount: -1}
		if status, err := readBoundedProcFile(filepath.Join(processRoot, "status"), 64<<10); err == nil {
			parseManagedStatus(status, &metadata)
		}
		if descriptors, err := os.ReadDir(filepath.Join(processRoot, "fd")); err == nil {
			metadata.FDCount = len(descriptors)
		}
		document.Processes = append(document.Processes, metadata)
		if len(document.Processes) == maxManagedProcessEntries {
			break
		}
	}
	sort.Slice(document.Processes, func(left, right int) bool { return document.Processes[left].PID < document.Processes[right].PID })
	encoded, _ := json.Marshal(document)
	return encoded
}

func readBoundedProcFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}

func parseManagedStatus(status []byte, metadata *managedProcessMetadata) {
	for _, line := range strings.Split(string(status), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		switch name {
		case "State":
			metadata.State = fields[0]
		case "PPid":
			metadata.PPID, _ = strconv.Atoi(fields[0])
		case "Threads":
			metadata.Threads, _ = strconv.Atoi(fields[0])
		case "VmRSS":
			metadata.RSSKiB, _ = strconv.ParseUint(fields[0], 10, 64)
		}
	}
}
