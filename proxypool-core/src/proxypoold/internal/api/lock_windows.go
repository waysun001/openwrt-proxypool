//go:build windows

package api

import (
	"net"
	"os"
	"syscall"
)

type endpointLock struct{ handle syscall.Handle }

func acquireEndpointLock(path string) (*endpointLock, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return &endpointLock{h}, nil
}
func (l *endpointLock) Close() error { return syscall.CloseHandle(l.handle) }
func listenUnixPrivate(path string) (*net.UnixListener, error) {
	return net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
}
