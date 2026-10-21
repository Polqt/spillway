//go:build windows

package config

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareBackup(ctx context.Context, _ string, _ []byte) error { return ctx.Err() }

func replaceFile(ctx context.Context, source, target, backup string) error {
	for attempt := 0; ; attempt++ {
		err := replaceFileWindows(source, target, backup)
		if err == nil || !retryableReplaceError(err) || attempt >= 5 {
			return err
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func replaceFileWindows(source, target, backup string) error {
	sourcePtr, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	backupPtr, err := windows.UTF16PtrFromString(backup)
	if err != nil {
		return err
	}
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	dll := windows.NewLazySystemDLL("kernel32.dll")
	proc := dll.NewProc("ReplaceFileW")
	if err := dll.Load(); err != nil {
		return fmt.Errorf("load ReplaceFileW: %w", err)
	}
	// #nosec G103 -- ReplaceFileW requires pointers to the UTF-16 paths and does not retain them.
	result, _, callErr := proc.Call(uintptr(unsafe.Pointer(targetPtr)), uintptr(unsafe.Pointer(sourcePtr)), uintptr(unsafe.Pointer(backupPtr)), 0, 0, 0)
	if result != 0 {
		return nil
	}
	if errors.Is(callErr, syscall.Errno(0)) {
		callErr = windows.GetLastError()
	}
	return fmt.Errorf("ReplaceFileW: %w", callErr)
}

func retryableReplaceError(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == windows.ERROR_SHARING_VIOLATION || errno == windows.ERROR_LOCK_VIOLATION)
}

func syncDirectory(ctx context.Context, _ string) error { return ctx.Err() }
