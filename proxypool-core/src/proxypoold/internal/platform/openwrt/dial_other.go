//go:build !linux

package openwrt

import (
	"context"
	"errors"
	"net"
)

func dialBoundDevice(context.Context, string, string, string) (net.Conn, error) {
	return nil, errors.New("device-bound dialing is unsupported")
}
