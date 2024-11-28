package flannel_network

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"time"
)

func readPipe(pipe io.Reader, doneChan chan struct{}) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(os.Stdout, line) // Write to os.Stdout
		if strings.Contains(line, "bootstrap done") {
			close(doneChan)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Println("Error reading pipe:", err)
	}
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if err.Error() == "os: process already finished" {
		return false
	}
	var errno syscall.Errno
	ok := errors.As(err, &errno)
	if !ok {
		return false
	}
	switch {
	case errors.Is(errno, syscall.ESRCH):
		return false
	case errors.Is(errno, syscall.EPERM):
		return true
	}
	return false
}

func waitForFileWithContext(ctx context.Context, path string) error {
	const pollInterval = 100 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			// Context has been canceled or timed out
			return fmt.Errorf("timed out waiting for file %s: %w", path, ctx.Err())
		default:
			// Continue to check for the file
		}

		// Attempt to get file info
		_, err := os.Stat(path)
		if err == nil {
			// File exists
			return nil
		}
		if !os.IsNotExist(err) {
			// An error other than "not exists" occurred
			return fmt.Errorf("error checking file %s: %w", path, err)
		}

		// Wait for the next polling interval or context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for file %s: %w", path, ctx.Err())
		case <-time.After(pollInterval):
			// Continue looping
		}
	}
}
