//go:build linux

package openwrt

import (
	"context"
	"net"
	"syscall"
)

func dialBoundDevice(ctx context.Context, network, address, device string) (net.Conn, error) {
	dialer := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
		var optionErr error
		if err := raw.Control(func(descriptor uintptr) {
			optionErr = syscall.SetsockoptString(int(descriptor), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, device)
		}); err != nil {
			return err
		}
		return optionErr
	}}
	return dialer.DialContext(ctx, network, address)
}
