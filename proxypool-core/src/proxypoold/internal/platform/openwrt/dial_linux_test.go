package openwrt

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestConfigureBoundSocketSetsPolicyMarkBeforeBindingDevice(t *testing.T) {
	var calls []string
	options := socketOptionSetters{
		setInt: func(fd, level, option, value int) error {
			if fd != 17 || level != syscall.SOL_SOCKET || option != syscall.SO_MARK || value != 0x005a002a {
				t.Fatalf("unexpected integer socket option: fd=%d level=%d option=%d value=%#x", fd, level, option, value)
			}
			calls = append(calls, "mark")
			return nil
		},
		setString: func(fd, level, option int, value string) error {
			if fd != 17 || level != syscall.SOL_SOCKET || option != syscall.SO_BINDTODEVICE || value != "l2tp-ppv20042" {
				t.Fatalf("unexpected string socket option: fd=%d level=%d option=%d value=%q", fd, level, option, value)
			}
			calls = append(calls, "device")
			return nil
		},
	}
	if err := configureBoundSocket(17, "l2tp-ppv20042", 0x005a002a, options); err != nil {
		t.Fatalf("configure bound socket: %v", err)
	}
	if len(calls) != 2 || calls[0] != "mark" || calls[1] != "device" {
		t.Fatalf("socket option order = %v", calls)
	}
}

func TestConfigureBoundSocketFailsClosedOnEitherSocketOption(t *testing.T) {
	markErr := errors.New("mark rejected")
	bindErr := errors.New("bind rejected")
	tests := []struct {
		name        string
		markError   error
		bindError   error
		want        error
		wantBinding bool
	}{
		{name: "mark", markError: markErr, want: markErr},
		{name: "device", bindError: bindErr, want: bindErr, wantBinding: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bindingCalled := false
			options := socketOptionSetters{
				setInt: func(_, _, _, _ int) error { return test.markError },
				setString: func(_, _, _ int, _ string) error {
					bindingCalled = true
					return test.bindError
				},
			}
			err := configureBoundSocket(17, "l2tp-ppv20042", 0x005a002a, options)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if bindingCalled != test.wantBinding {
				t.Fatalf("binding called = %t, want %t", bindingCalled, test.wantBinding)
			}
		})
	}
}

func TestConfigureBoundSocketAppliesRealLinuxSocketOptions(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("SO_MARK verification requires root")
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("create socket: %v", err)
	}
	defer syscall.Close(fd)

	const mark = 0x005a002a
	if err := configureBoundSocket(uintptr(fd), "lo", mark, systemSocketOptionSetters); err != nil {
		t.Fatalf("configure real socket: %v", err)
	}
	gotMark, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_MARK)
	if err != nil || gotMark != mark {
		t.Fatalf("SO_MARK = %#x, %v; want %#x", gotMark, err, mark)
	}

	device := make([]byte, 32)
	deviceLength := uint32(len(device))
	_, _, errno := syscall.Syscall6(
		syscall.SYS_GETSOCKOPT,
		uintptr(fd), uintptr(syscall.SOL_SOCKET), uintptr(syscall.SO_BINDTODEVICE),
		uintptr(unsafe.Pointer(&device[0])), uintptr(unsafe.Pointer(&deviceLength)), 0,
	)
	if errno != 0 {
		t.Fatalf("read SO_BINDTODEVICE: %v", errno)
	}
	gotDevice := strings.TrimRight(string(device[:deviceLength]), "\x00")
	if gotDevice != "lo" {
		t.Fatalf("SO_BINDTODEVICE = %q, want lo", gotDevice)
	}
}
