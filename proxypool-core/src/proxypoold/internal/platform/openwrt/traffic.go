package openwrt

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"proxypoold/internal/platform"
)

const maxCounterFileBytes = 64

var interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

type SysfsTrafficReader struct {
	root string
}

func NewSysfsTrafficReader(root string) *SysfsTrafficReader {
	return &SysfsTrafficReader{root: filepath.Clean(root)}
}

func (reader *SysfsTrafficReader) ReadInterfaceCounters(interfaceName string) (platform.InterfaceCounters, error) {
	if reader == nil || reader.root == "" || reader.root == "." || !interfaceNamePattern.MatchString(interfaceName) {
		return platform.InterfaceCounters{}, errors.New("interface traffic reader is unavailable")
	}
	statistics := filepath.Join(reader.root, interfaceName, "statistics")
	rxBytes, err := readCounterFile(filepath.Join(statistics, "rx_bytes"))
	if err != nil {
		return platform.InterfaceCounters{}, errors.New("interface receive counter is unavailable")
	}
	txBytes, err := readCounterFile(filepath.Join(statistics, "tx_bytes"))
	if err != nil {
		return platform.InterfaceCounters{}, errors.New("interface transmit counter is unavailable")
	}
	return platform.InterfaceCounters{RXBytes: rxBytes, TXBytes: txBytes}, nil
}

func readCounterFile(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("counter path is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxCounterFileBytes+1))
	if err != nil || len(contents) == 0 || len(contents) > maxCounterFileBytes {
		return 0, errors.New("counter value is invalid")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" {
		return 0, errors.New("counter value is empty")
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, errors.New("counter value is not an unsigned integer")
	}
	return parsed, nil
}
