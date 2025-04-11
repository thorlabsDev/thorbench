package internal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/charmbracelet/log"
)

// BufferedWriter wraps an io.Writer and provides buffering with manual flushing
type BufferedWriter struct {
	w      io.Writer
	mu     sync.Mutex
	buffer []byte
}

// NewBufferedWriter creates a new BufferedWriter
func NewBufferedWriter(w io.Writer) *BufferedWriter {
	return &BufferedWriter{
		w:      w,
		buffer: make([]byte, 0, 4096),
	}
}

// Write implements io.Writer
func (bw *BufferedWriter) Write(p []byte) (n int, err error) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	// Add to buffer
	bw.buffer = append(bw.buffer, p...)

	// If buffer exceeds 2KB or includes a newline, flush it
	if len(bw.buffer) > 2048 || containsNewline(p) {
		err = bw.flush()
		if err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

// Flush forces a write of all buffered data
func (bw *BufferedWriter) Flush() error {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	return bw.flush()
}

// flush without locking (must be called with lock held)
func (bw *BufferedWriter) flush() error {
	if len(bw.buffer) == 0 {
		return nil
	}

	_, err := bw.w.Write(bw.buffer)
	if err != nil {
		return err
	}

	// If underlying writer is a file, try to sync it
	if f, ok := bw.w.(*os.File); ok {
		_ = f.Sync()
	}

	bw.buffer = bw.buffer[:0]
	return nil
}

// containsNewline checks if byte slice contains a newline
func containsNewline(p []byte) bool {
	for _, b := range p {
		if b == '\n' {
			return true
		}
	}
	return false
}

// Our log file and buffered writer
var (
	logFile        *os.File
	bufferedWriter *BufferedWriter
	stopFlusher    chan struct{}
	flushInterval  = 1 * time.Second
)

// SetupLogger initializes the logging system with both console and file output
func SetupLogger() {
	// First, set up a basic console logger for initial operations
	SimpleLogger = log.NewWithOptions(os.Stdout, log.Options{
		ReportTimestamp: false,
	})

	// Create the log filename with timestamp and TestID
	LogFileName = fmt.Sprintf("thor_bench_%d_%s.log", time.Now().UnixMilli(), TestID)

	// Get executable directory for log file
	execDir := getExecutablePath()

	// Create full log path in same directory as executable
	logPath := filepath.Join(execDir, LogFileName)

	// Create or open the log file
	var err error
	logFile, err = os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		SimpleLogger.Printf("Error opening log file at %s: %v\nFalling back to current directory.", logPath, err)
		// Fallback to current directory
		logFile, err = os.OpenFile(LogFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			fmt.Printf("Fatal error: Could not create log file: %v\n", err)
			os.Exit(1)
		}
	}

	// Create buffered writer for the file
	bufferedWriter = NewBufferedWriter(logFile)

	// Create a multi-writer for both console and buffered file output
	multi := io.MultiWriter(os.Stdout, bufferedWriter)

	// Update the SimpleLogger to use both outputs
	SimpleLogger = log.NewWithOptions(multi, log.Options{
		ReportTimestamp: true,
		TimeFunction:    func(time.Time) time.Time { return time.Now().UTC() },
		TimeFormat:      "15:04:05.000",
	})

	// Set up the default logger as well
	log.SetDefault(log.NewWithOptions(multi, log.Options{
		Prefix:          TestID,
		ReportTimestamp: true,
		TimeFunction:    func(time.Time) time.Time { return time.Now().UTC() },
		TimeFormat:      "15:04:05.000",
	}))

	// Start background flusher
	stopFlusher = make(chan struct{})
	go periodicFlusher(flushInterval, stopFlusher)

	SimpleLogger.Printf("Log file created at: %s", logPath)
	SimpleLogger.Printf("Automatic log flushing enabled with %s interval", flushInterval)
}

// periodicFlusher flushes the log buffer at regular intervals
func periodicFlusher(interval time.Duration, stop chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			FlushLogs()
		case <-stop:
			FlushLogs() // Final flush
			return
		}
	}
}

// FlushLogs forces all buffered log data to be written to disk
func FlushLogs() {
	if bufferedWriter != nil {
		if err := bufferedWriter.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "Error flushing logs: %v\n", err)
		}
	}
}

// CleanupLogger properly closes log resources
func CleanupLogger() {
	// Stop the flusher goroutine
	if stopFlusher != nil {
		close(stopFlusher)
		stopFlusher = nil
	}

	// Final flush and close the file
	FlushLogs()

	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}
