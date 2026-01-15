package flood

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	pidFileName = "evmbombard.pid"
)

// Config holds the configuration for flooding
type Config struct {
	RPCURL    string   // Single RPC URL (for backwards compat)
	RPCURLs   []string // Multiple RPC URLs for load balancing
	KeysCount int
	BatchSize int
	DataDir   string
}

// Start starts the evmbombard process
func Start(ctx context.Context, cfg Config) error {
	// Check if already running
	if isRunning(cfg.DataDir) {
		return fmt.Errorf("flooding already in progress (use 'benchmark stop-flood' to stop)")
	}

	// Find evmbombard binary
	evmbombardPath, err := findEvmbombard()
	if err != nil {
		return fmt.Errorf("evmbombard not found: %w", err)
	}

	// Determine RPC URLs to use
	var rpcURLsArg string
	if len(cfg.RPCURLs) > 0 {
		rpcURLsArg = strings.Join(cfg.RPCURLs, ",")
	} else {
		rpcURLsArg = cfg.RPCURL
	}

	fmt.Printf("Using evmbombard: %s\n", evmbombardPath)
	fmt.Printf("RPC URLs: %s\n", rpcURLsArg)
	fmt.Printf("Keys: %d, Batch: %d\n", cfg.KeysCount, cfg.BatchSize)

	// Build command
	cmd := exec.Command(
		evmbombardPath,
		"-rpc", rpcURLsArg,
		"-keys", strconv.Itoa(cfg.KeysCount),
		"-batch", strconv.Itoa(cfg.BatchSize),
	)

	// Set working directory for keys file
	cmd.Dir = cfg.DataDir

	// Redirect output to files
	logPath := filepath.Join(cfg.DataDir, "evmbombard.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Start process
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start evmbombard: %w", err)
	}

	fmt.Printf("evmbombard started (PID: %d)\n", cmd.Process.Pid)
	fmt.Printf("Log file: %s\n", logPath)

	// Save PID
	if err := savePID(cfg.DataDir, cmd.Process.Pid); err != nil {
		// Try to kill the process if we can't save the PID
		cmd.Process.Kill()
		logFile.Close()
		return fmt.Errorf("failed to save PID: %w", err)
	}

	// Don't wait for the process - it runs in background
	go func() {
		cmd.Wait()
		logFile.Close()
		removePID(cfg.DataDir)
	}()

	return nil
}

// Stop stops the evmbombard process
func Stop(dataDir string) error {
	pid, err := loadPID(dataDir)
	if err != nil {
		return fmt.Errorf("no flooding process found: %w", err)
	}

	// Find the process
	process, err := os.FindProcess(pid)
	if err != nil {
		removePID(dataDir)
		return fmt.Errorf("process not found: %w", err)
	}

	// Send SIGTERM for graceful shutdown
	fmt.Printf("Stopping evmbombard (PID: %d)...\n", pid)
	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Process might already be dead
		removePID(dataDir)
		return nil
	}

	// Remove PID file
	removePID(dataDir)

	fmt.Println("evmbombard stopped")
	return nil
}

// IsRunning checks if evmbombard is running
func isRunning(dataDir string) bool {
	pid, err := loadPID(dataDir)
	if err != nil {
		return false
	}

	// Check if process exists
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Send signal 0 to check if process is running
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func findEvmbombard() (string, error) {
	homeDir, _ := os.UserHomeDir()

	// Get executable directory for bundled binary
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)

	// Check common locations (bundled bombard takes priority)
	locations := []string{
		// Bundled binary (same directory as benchmark)
		filepath.Join(exeDir, "bombard"),
		// Local binary
		"./bin/bombard",
		"./bin/evmbombard",
		"./bombard",
		"./evmbombard",
		// User's go bin (both GOPATH and default ~/go)
		filepath.Join(os.Getenv("GOPATH"), "bin", "evmbombard"),
		filepath.Join(homeDir, "go", "bin", "evmbombard"),
		// System path
		"/usr/local/bin/evmbombard",
	}

	// Also check EVMBOMBARD_PATH env var
	if envPath := os.Getenv("EVMBOMBARD_PATH"); envPath != "" {
		locations = append([]string{envPath}, locations...)
	}

	// Also check PATH for both names
	if path, err := exec.LookPath("bombard"); err == nil {
		return path, nil
	}
	if path, err := exec.LookPath("evmbombard"); err == nil {
		return path, nil
	}

	for _, loc := range locations {
		absPath, err := filepath.Abs(loc)
		if err != nil {
			continue
		}
		if _, err := os.Stat(absPath); err == nil {
			return absPath, nil
		}
	}

	return "", fmt.Errorf("bombard binary not found. Run 'make build' to build it, or set EVMBOMBARD_PATH")
}

type pidFile struct {
	PID int `json:"pid"`
}

func savePID(dataDir string, pid int) error {
	pidPath := filepath.Join(dataDir, pidFileName)
	data, err := json.Marshal(pidFile{PID: pid})
	if err != nil {
		return err
	}
	return os.WriteFile(pidPath, data, 0644)
}

func loadPID(dataDir string) (int, error) {
	pidPath := filepath.Join(dataDir, pidFileName)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, err
	}
	var pf pidFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return 0, err
	}
	return pf.PID, nil
}

func removePID(dataDir string) {
	pidPath := filepath.Join(dataDir, pidFileName)
	os.Remove(pidPath)
}

// Status returns whether flooding is running and its PID
func Status(dataDir string) (bool, int) {
	pid, err := loadPID(dataDir)
	if err != nil {
		return false, 0
	}

	// Check if process exists
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, 0
	}

	// Send signal 0 to check if process is running
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		return false, 0
	}

	return true, pid
}
