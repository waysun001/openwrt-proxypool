//go:build windows

package config

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

const (
	windowsSharingViolation syscall.Errno = 32
	windowsLockViolation    syscall.Errno = 33
)

type storeTransactionLock struct {
	handle syscall.Handle
}

func acquireStoreTransactionLock(ctx context.Context, path string) (*storeTransactionLock, error) {
	for {
		lock, err := tryAcquireStoreTransactionLock(path)
		if err == nil || !errors.Is(err, errStoreTransactionBusy) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func tryAcquireStoreTransactionLock(path string) (*storeTransactionLock, error) {
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, errors.New("configuration transaction lock path is invalid")
	}
	handle, openErr := syscall.CreateFile(pointer, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if openErr == nil {
		_ = os.Chmod(path, 0o600)
		return &storeTransactionLock{handle: handle}, nil
	}
	if openErr == windowsSharingViolation || openErr == windowsLockViolation {
		return nil, errStoreTransactionBusy
	}
	return nil, errors.New("configuration transaction lock failed")
}

func (lock *storeTransactionLock) Close() error {
	return syscall.CloseHandle(lock.handle)
}
