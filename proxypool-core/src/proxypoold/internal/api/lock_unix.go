//go:build !windows

package api

import (
	"net"
	"os"
	"sync"
	"syscall"
)

type endpointLock struct{ file *os.File }

func acquireEndpointLock(path string) (*endpointLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &endpointLock{f}, nil
}
func (l *endpointLock) Close() error {
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

var umaskMu sync.Mutex

func listenUnixPrivate(path string) (*net.UnixListener, error) {
	umaskMu.Lock()
	old := syscall.Umask(0o177)
	defer func() { syscall.Umask(old); umaskMu.Unlock() }()
	return net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
}
