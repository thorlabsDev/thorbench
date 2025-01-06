package internal

import (
	"math"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/montanaflynn/stats"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type TxStat struct {
	Delta  time.Duration
	Status string // "SUCCESS", "FAILED", or "SLIPPAGE"
}

var TxStats []TxStat

func PrintStatsAndBlocks() {
	// Create color helpers
	cyan := color.New(color.FgCyan).SprintFunc()
	green := color.New(color.FgGreen).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()

	// Print core statistics header
	SimpleLogger.Print("")
	SimpleLogger.Print(cyan("Core Statistics"))
	SimpleLogger.Print(strings.Repeat("=", 50))

	// Print transaction counts
	SimpleLogger.Printf("%s %d", green("Transactions Succeeded :"), ProcessedTransactions)
	SimpleLogger.Printf("%s %d (Slippage=%d, Other=%d)",
		red("Transactions Failed    :"),
		SlippageFailures+OtherFailures,
		SlippageFailures,
		OtherFailures,
	)

	notLanded := SentTransactions - (ProcessedTransactions + SlippageFailures + OtherFailures)
	if notLanded < 0 {
		notLanded = 0
	}
	SimpleLogger.Printf("%s %d", yellow("Not Landed or Time-Out :"), notLanded)
	SimpleLogger.Printf("%s %d", cyan("Total Attempted        :"), SentTransactions)

	// Calculate and print success rates if we had any transactions
	if GlobalConfig.TxCount > 0 {
		SimpleLogger.Print("")
		SimpleLogger.Print(cyan("Success Rates"))
		SimpleLogger.Print(strings.Repeat("-", 30))

		// Calculate effective success rate
		effectiveRate := float64(ProcessedTransactions) / float64(GlobalConfig.TxCount) * 100

		// Calculate overall landing rate
		overallRate := float64(0)
		if SentTransactions > 0 {
			overallRate = 100.0 * float64(ProcessedTransactions+SlippageFailures+OtherFailures) /
				float64(SentTransactions)
		}

		SimpleLogger.Printf("%s %.1f%%", cyan("Effective Success Rate :"), effectiveRate)
		SimpleLogger.Printf("%s %.1f%%", cyan("Overall Landing Rate  :"), overallRate)
	}

	// Separate timing statistics by status
	var successDeltas []float64
	var failDeltas []float64
	var slipDeltas []float64

	for _, s := range TxStats {
		ns := float64(s.Delta.Nanoseconds())
		switch s.Status {
		case "SUCCESS":
			successDeltas = append(successDeltas, ns)
		case "FAILED":
			failDeltas = append(failDeltas, ns)
		case "SLIPPAGE":
			slipDeltas = append(slipDeltas, ns)
		}
	}

	// Print detailed timing statistics
	SimpleLogger.Print("")
	SimpleLogger.Print(cyan("Timing Statistics"))
	SimpleLogger.Print(strings.Repeat("=", 50))

	if len(successDeltas) > 0 {
		printStatsForStatus("Successful", successDeltas)
	}
	if len(failDeltas) > 0 {
		printStatsForStatus("Failed", failDeltas)
	}
	if len(slipDeltas) > 0 {
		printStatsForStatus("Slippage", slipDeltas)
	}

	// Print block distribution
	SimpleLogger.Print("")
	SimpleLogger.Print(cyan("Block Distribution"))
	SimpleLogger.Print(strings.Repeat("=", 50))

	DisplayBlocks()
}

func printStatsForStatus(txType string, deltas []float64) {
	minVal, _ := stats.Min(deltas)
	maxVal, _ := stats.Max(deltas)
	avg, _ := stats.Mean(deltas)
	median, _ := stats.Median(deltas)
	p90, _ := stats.Percentile(deltas, 90)
	p95, _ := stats.Percentile(deltas, 95)
	p99, _ := stats.Percentile(deltas, 99)

	SimpleLogger.Printf("%s Transactions (%d total):", txType, len(deltas))
	SimpleLogger.Printf("  Minimum Time        : %v", time.Duration(minVal).Truncate(time.Millisecond))
	SimpleLogger.Printf("  Maximum Time        : %v", time.Duration(maxVal).Truncate(time.Millisecond))
	SimpleLogger.Printf("  Average Time        : %v", time.Duration(avg).Truncate(time.Millisecond))
	SimpleLogger.Printf("  Median Time         : %v", time.Duration(median).Truncate(time.Millisecond))
	SimpleLogger.Printf("  90th Percentile     : %v", time.Duration(p90).Truncate(time.Millisecond))
	SimpleLogger.Printf("  95th Percentile     : %v", time.Duration(p95).Truncate(time.Millisecond))
	SimpleLogger.Printf("  99th Percentile     : %v", time.Duration(p99).Truncate(time.Millisecond))
	SimpleLogger.Print("")
}

func DisplayBlocks() {
	if len(TxBlocks) == 0 {
		SimpleLogger.Print("No block distribution data available.")
		return
	}

	var first uint64 = math.MaxUint64
	var last uint64
	for block := range TxBlocks {
		if block < first {
			first = block
		}
		if block > last {
			last = block
		}
	}

	printer := message.NewPrinter(language.English)
	SimpleLogger.Printf("Distribution across %d blocks:", last-first+1)
	SimpleLogger.Print("")

	for block := first; block <= last; block++ {
		count, ok := TxBlocks[block]
		if !ok {
			continue
		}

		var percentage float64
		if ProcessedTransactions > 0 {
			percentage = float64(count) / float64(ProcessedTransactions) * 100
		}
		stars := int(math.Ceil(percentage))

		SimpleLogger.Printf("Block %s : %3d txns | %5.1f%% | %s",
			printer.Sprintf("%d", block),
			count,
			percentage,
			strings.Repeat("*", stars),
		)
	}
}
