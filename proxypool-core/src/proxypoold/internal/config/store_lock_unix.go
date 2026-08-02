//go:build !windows

package config

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

type storeTransactionLock struct {
	file *os.File
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
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("configuration transaction lock open failed")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("configuration transaction lock open failed")
	}
	_ = file.Chmod(0o600)
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return &storeTransactionLock{file: file}, nil
	}
	_ = file.Close()
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return nil, errStoreTransactionBusy
	}
	return nil, errors.New("configuration transaction lock failed")
}

func (lock *storeTransactionLock) Close() error {
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	return lock.file.Close()
}
