//go:build linux

package openwrt

import (
	"context"
	"net"
	"syscall"
)

type socketOptionSetters struct {
	setInt    func(int, int, int, int) error
	setString func(int, int, int, string) error
}

var systemSocketOptionSetters = socketOptionSetters{
	setInt:    syscall.SetsockoptInt,
	setString: syscall.SetsockoptString,
}

func configureBoundSocket(descriptor uintptr, device string, mark uint32, setters socketOptionSetters) error {
	if err := setters.setInt(int(descriptor), syscall.SOL_SOCKET, syscall.SO_MARK, int(mark)); err != nil {
		return err
	}
	return setters.setString(int(descriptor), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, device)
}

func dialBoundDevice(ctx context.Context, network, address, device string, mark uint32) (net.Conn, error) {
	dialer := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
		var optionErr error
		if err := raw.Control(func(descriptor uintptr) {
			optionErr = configureBoundSocket(descriptor, device, mark, systemSocketOptionSetters)
		}); err != nil {
			return err
		}
		return optionErr
	}}
	return dialer.DialContext(ctx, network, address)
}
