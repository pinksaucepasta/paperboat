//go:build windows

package pty

import (
	"errors"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/windows"
)

var cancelInputIO = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

// WriteInput cancels only the synchronous IO of this locked thread. The
// cancellation goroutine finishes before the thread can be reused by Go.
func WriteInput(file *os.File, data []byte) (int, error) {
	return writeInput(file, data, 20*time.Second)
}
func writeInput(file *os.File, data []byte, timeout time.Duration) (int, error) {
	if err := cancelInputIO.Find(); err != nil {
		return 0, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(thread)
	done, cancelled := make(chan struct{}), make(chan struct{})
	timer := time.NewTimer(timeout)
	go func() {
		defer close(cancelled)
		select {
		case <-done:
		case <-timer.C:
			// The deadline can win just before WriteFile enters the kernel. Retry
			// until this write completes; a single ERROR_NOT_FOUND must not leave a
			// subsequently started synchronous pipe write unbounded.
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				default:
				}
				_, _, _ = cancelInputIO.Call(uintptr(thread))
				select {
				case <-done:
					return
				case <-ticker.C:
				}
			}
		}
	}()
	n, err := file.Write(data)
	close(done)
	timer.Stop()
	<-cancelled
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
		return n, os.ErrDeadlineExceeded
	}
	return n, err
}
