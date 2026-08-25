//go:build !linux

package openwrt

import (
	"context"
	"errors"
	"net"
)

func dialBoundDevice(context.Context, string, string, string, uint32) (net.Conn, error) {
	return nil, errors.New("device-bound dialing is unsupported")
}
