package main

import (
	"fmt"
	"github.com/coreos/go-systemd/v22/journal"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	sandboxesDir  = "/var/run/flannel-np/sandboxes"
	readyDir      = "/var/run/flannel-np/ready"
	netnsDir      = "/var/run/docker/netns"
	sleepInterval = 100 * time.Millisecond
	maxWaitTime   = 20 * time.Second
)

// logMessage writes log messages to a file and stdout
func logMessage(message string) {
	journal.Send(message, journal.PriDebug, map[string]string{
		"SYSLOG_IDENTIFIER": "flannel-network-plugin-hook"
	})
}

// getCurrentNetNS gets the network namespace ID of the running process
func getCurrentNetNS() (string, error) {
	link, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return "", fmt.Errorf("failed to get current network namespace: %w", err)
	}

	// Extract the namespace ID from the link string
	parts := strings.Split(link, "[")
	if len(parts) < 2 {
		return "", fmt.Errorf("unexpected format for network namespace: %s", link)
	}

	nsID := strings.TrimSuffix(parts[1], "]")
	return nsID, nil
}

// getSandboxNetNS gets the network namespace ID of a given sandbox
func getSandboxNetNS(sandboxID string) (string, error) {
	netnsPath := filepath.Join(netnsDir, sandboxID)

	// Ensure the network namespace file exists
	if _, err := os.Stat(netnsPath); os.IsNotExist(err) {
		return "", fmt.Errorf("no docker netns file found for %s", sandboxID)
	}

	// Get inode number using `ls -i` equivalent
	fileInfo, err := os.Stat(netnsPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat netns file for %s: %w", sandboxID, err)
	}

	return strconv.FormatUint(uint64(fileInfo.Sys().(*syscall.Stat_t).Ino), 10), nil
}

func main() {
	startTime := time.Now()

	os.MkdirAll(sandboxesDir, 0755)
	os.MkdirAll(readyDir, 0755)

	// Get current network namespace ID
	currentNetNS, err := getCurrentNetNS()
	if err != nil {
		logMessage(fmt.Sprintf("Error: %v", err))
		os.Exit(1)
	}

	logMessage(fmt.Sprintf("Container Network namespace: %s", currentNetNS))

	// Iterate over all sandbox files
	sandboxFiles, err := os.ReadDir(sandboxesDir)
	if err != nil {
		logMessage(fmt.Sprintf("Failed to read sandbox directory: %v", err))
		os.Exit(1)
	}

	for _, sandboxFile := range sandboxFiles {
		sandboxID := sandboxFile.Name()
		sandboxNetNS, err := getSandboxNetNS(sandboxID)
		if err != nil {
			//logMessage(fmt.Sprintf("Skipping sandbox %s: %v", sandboxID, err))
			continue
		}

		//logMessage(fmt.Sprintf("Sandbox %s has network namespace %s.", sandboxID, sandboxNetNS))

		// Compare namespace IDs
		if currentNetNS == sandboxNetNS {
			readyFile := filepath.Join(readyDir, sandboxID)

			logMessage(fmt.Sprintf("Waiting for network initialization for %s: Waiting for existence of file %s...", sandboxID, readyFile))

			// Wait for the ready file to be created
			for {
				if _, err := os.Stat(readyFile); err == nil {
					break
				}
				time.Sleep(sleepInterval)
				if time.Since(startTime) > maxWaitTime {
					logMessage(fmt.Sprintf("Timeout while waiting for %s. Exiting hook anyway, to prevent deadlock", readyFile))
					os.Exit(0)
				}
			}

			duration := time.Since(startTime)
			logMessage(fmt.Sprintf("Network initialization complete. Waited %s.", duration))

			// Cleanup files if they still exist
			if err := os.Remove(readyFile); err == nil {
				logMessage(fmt.Sprintf("Deleted %s", readyFile))
			}

			sandboxFilePath := filepath.Join(sandboxesDir, sandboxID)
			if err := os.Remove(sandboxFilePath); err == nil {
				logMessage(fmt.Sprintf("Deleted %s", sandboxFilePath))
			}

			os.Exit(0)
		}
	}

	// If no match was found, exit immediately
	logMessage("No matching sandbox found. Exiting.")
	os.Exit(0)
}
