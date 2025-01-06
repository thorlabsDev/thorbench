package internal

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/charmbracelet/log"
)

func SetupLogger() {
	LogFileName = fmt.Sprintf("thor_bench_%d_%s.log", time.Now().UnixMilli(), TestID)
	logFile, err := os.OpenFile(LogFileName, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		fmt.Printf("error opening log file: %v\n", err)
		os.Exit(1)
	}

	multi := io.MultiWriter(os.Stdout, logFile)

	SimpleLogger = log.NewWithOptions(multi, log.Options{
		ReportTimestamp: false,
	})

	log.SetDefault(log.NewWithOptions(multi, log.Options{
		Prefix:          TestID,
		ReportTimestamp: true,
		TimeFunction:    func(time.Time) time.Time { return time.Now().UTC() },
		TimeFormat:      "15:04:05.0000",
	}))
}
