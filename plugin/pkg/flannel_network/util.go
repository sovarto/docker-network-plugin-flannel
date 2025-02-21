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
	"time"
)

func readPipe(pipe io.Reader, doneChan chan struct{}) {
	channelClosed := false
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Printf("[flanneld] %s\n", line)
		if strings.Contains(line, "bootstrap done") && !channelClosed {
			channelClosed = true
			close(doneChan)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		log.Println("Error reading pipe:", err)
	}
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
