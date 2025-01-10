package internal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/log"
)

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
	logFile, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		SimpleLogger.Printf("Error opening log file at %s: %v\nFalling back to current directory.", logPath, err)
		// Fallback to current directory
		logFile, err = os.OpenFile(LogFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			fmt.Printf("Fatal error: Could not create log file: %v\n", err)
			os.Exit(1)
		}
	}

	// Create a multi-writer for both console and file output
	multi := io.MultiWriter(os.Stdout, logFile)

	// Update the SimpleLogger to use both outputs
	SimpleLogger = log.NewWithOptions(multi, log.Options{
		ReportTimestamp: false,
	})

	// Set up the default logger as well
	log.SetDefault(log.NewWithOptions(multi, log.Options{
		Prefix:          TestID,
		ReportTimestamp: true,
		TimeFunction:    func(time.Time) time.Time { return time.Now().UTC() },
		TimeFormat:      "15:04:05.0000",
	}))

	SimpleLogger.Printf("Log file created at: %s", logPath)
}
