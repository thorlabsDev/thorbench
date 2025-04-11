package internal

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/montanaflynn/stats"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type TxStat struct {
	Delta           time.Duration
	Status          string // "SUCCESS", "FAILED", or "SLIPPAGE"
	SendBlockHeight uint64
	LandBlockHeight uint64
	BlockHeightDiff uint64
	BlockTime       time.Duration
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

	// Fix integer overflow in notLanded calculation
	var notLanded uint64
	totalLanded := ProcessedTransactions + SlippageFailures + OtherFailures
	if SentTransactions > totalLanded {
		notLanded = SentTransactions - totalLanded
	}

	SimpleLogger.Printf("%s %d", yellow("Not Landed or Time-Out :"), notLanded)
	SimpleLogger.Printf("%s %d", cyan("Total Attempted        :"), SentTransactions)
	SimpleLogger.Printf("%s %d", cyan("Target Transaction Count:"), GlobalConfig.TxCount)

	// Calculate and print success rates if we had any transactions
	if GlobalConfig.TxCount > 0 {
		SimpleLogger.Print("")
		SimpleLogger.Print(cyan("Success Rates"))
		SimpleLogger.Print(strings.Repeat("-", 30))

		// Calculate effective success rate against target count
		effectiveRate := float64(ProcessedTransactions) / float64(GlobalConfig.TxCount) * 100

		// Calculate overall landing rate against sent transactions
		overallRate := float64(0)
		if SentTransactions > 0 {
			overallRate = float64(totalLanded) / float64(SentTransactions) * 100
		}

		SimpleLogger.Printf("%s %.1f%%", cyan("Effective Success Rate :"), effectiveRate)
		SimpleLogger.Printf("%s %.1f%%", cyan("Overall Landing Rate  :"), overallRate)
	}

	// Separate timing statistics by status
	var successDeltas []float64
	var failDeltas []float64
	var slipDeltas []float64

	// Block height difference statistics
	var blockHeightDiffs []float64
	var blockTimes []float64

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

		// Collect block height differences and block times for all transactions
		if s.BlockHeightDiff > 0 {
			blockHeightDiffs = append(blockHeightDiffs, float64(s.BlockHeightDiff))
			blockTimes = append(blockTimes, float64(s.BlockTime.Nanoseconds()))
		}
	}

	// Print detailed timing statistics
	SimpleLogger.Print("")
	SimpleLogger.Print(cyan("Timing Statistics (Websocket Retrieval)"))
	SimpleLogger.Print(strings.Repeat("=", 50))
	SimpleLogger.Print("Note: These times measure how long it took to receive confirmation via websocket,")
	SimpleLogger.Print("      primarily reflect client network latency and connection quality to the RPC node, in addition to blockchain confirmation time.")

	if len(successDeltas) > 0 {
		printStatsForStatus("Successful", successDeltas)
	}
	if len(failDeltas) > 0 {
		printStatsForStatus("Failed", failDeltas)
	}
	if len(slipDeltas) > 0 {
		printStatsForStatus("Slippage", slipDeltas)
	}

	// Print block height difference statistics
	// Print block height difference statistics
	if len(blockHeightDiffs) > 0 {
		SimpleLogger.Print("")
		SimpleLogger.Print(cyan("Block Statistics (Blockchain Confirmation)"))
		SimpleLogger.Print(strings.Repeat("=", 50))
		SimpleLogger.Print("Note: These statistics reflect actual blockchain inclusion time")
		SimpleLogger.Print("      Consider that block measurements depend on RPC node's response time")

		minDiff, _ := stats.Min(blockHeightDiffs)
		maxDiff, _ := stats.Max(blockHeightDiffs)
		avgDiff, _ := stats.Mean(blockHeightDiffs)
		medianDiff, _ := stats.Median(blockHeightDiffs)
		p90Diff, _ := stats.Percentile(blockHeightDiffs, 90)
		p95Diff, _ := stats.Percentile(blockHeightDiffs, 95)
		p99Diff, _ := stats.Percentile(blockHeightDiffs, 99)

		// Count how many valid measurements we have
		validMeasurements := 0
		for _, diff := range blockHeightDiffs {
			if diff > 0 {
				validMeasurements++
			}
		}

		// Assuming average Solana block time of ~400ms
		const avgSolanaBlockTimeMs = 400.0

		SimpleLogger.Printf("Block Height Differences (%d/%d transactions):", validMeasurements, len(blockHeightDiffs))
		SimpleLogger.Printf("  Minimum Blocks        : %.0f", minDiff)
		SimpleLogger.Printf("  Maximum Blocks        : %.0f", maxDiff)
		SimpleLogger.Printf("  Average Blocks        : %.2f", avgDiff)
		SimpleLogger.Printf("  Median Blocks         : %.0f", medianDiff)
		SimpleLogger.Printf("  90th Percentile       : %.0f", p90Diff)
		SimpleLogger.Printf("  95th Percentile       : %.0f", p95Diff)
		SimpleLogger.Printf("  99th Percentile       : %.0f", p99Diff)
		SimpleLogger.Printf("  Est. Time (400ms/block): %.2fs", avgDiff*avgSolanaBlockTimeMs/1000)

		// Calculate estimated times for percentiles as well
		SimpleLogger.Printf("  Median Time (est.)    : %.2fs", medianDiff*avgSolanaBlockTimeMs/1000)
		SimpleLogger.Printf("  90th Time (est.)      : %.2fs", p90Diff*avgSolanaBlockTimeMs/1000)
		SimpleLogger.Printf("  95th Time (est.)      : %.2fs", p95Diff*avgSolanaBlockTimeMs/1000)
		SimpleLogger.Printf("  99th Time (est.)      : %.2fs", p99Diff*avgSolanaBlockTimeMs/1000)

		// Also calculate correlation between block differences and actual transaction times
		if validMeasurements > 1 {
			var blockDiffTimes []float64
			var actualTimes []float64

			for i, diff := range blockHeightDiffs {
				if diff > 0 {
					blockDiffTimes = append(blockDiffTimes, diff*avgSolanaBlockTimeMs/1000)

					// Only access transaction stats if we have a valid index
					var txTime float64

					// Find the corresponding transaction time for this block height diff
					if i < len(TxStats) {
						txTime = float64(TxStats[i].Delta.Nanoseconds()) / 1e9
					}

					actualTimes = append(actualTimes, txTime)
				}
			}

			if len(blockDiffTimes) > 1 && len(blockDiffTimes) == len(actualTimes) {
				correlation, err := stats.Correlation(blockDiffTimes, actualTimes)
				if err == nil {
					SimpleLogger.Printf("  Block/Time Correlation : %.2f", correlation)
				}
			}
		}

		// Calculate average observed block time if we have valid block times
		if len(blockTimes) > 0 {
			avgBlockTime, _ := stats.Mean(blockTimes)
			if avgBlockTime > 0 {
				SimpleLogger.Printf("  Observed Block Time   : %v", time.Duration(avgBlockTime).Truncate(time.Millisecond))
			}
		}

		SimpleLogger.Print("")
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

	var firstBlock uint64 = math.MaxUint64
	var lastBlock uint64
	for block := range TxBlocks {
		if block < firstBlock {
			firstBlock = block
		}
		if block > lastBlock {
			lastBlock = block
		}
	}

	printer := message.NewPrinter(language.English)
	totalBlocks := lastBlock - firstBlock + 1
	SimpleLogger.Printf("Distribution across %d blocks:", totalBlocks)
	SimpleLogger.Print("")

	// Fixed width format string to ensure alignment
	const formatStr = "Block %-12s : %5s %-4s | %5.1f%% | %s"

	for block := firstBlock; block <= lastBlock; block++ {
		count, exists := TxBlocks[block]
		if !exists {
			// Display empty blocks with 0 transactions
			SimpleLogger.Printf(formatStr,
				printer.Sprintf("%d", block),
				"No", "txns",
				0.0,
				"")
			continue
		}

		var percentage float64
		if ProcessedTransactions > 0 {
			percentage = float64(count) / float64(ProcessedTransactions) * 100
		}
		stars := int(math.Ceil(percentage))

		// Add block time if available
		blockTimeStr := ""
		if blockTime, ok := BlockTimes[block]; ok {
			blockTimeStr = blockTime.Format("15:04:05.000")
		}

		SimpleLogger.Printf(formatStr,
			printer.Sprintf("%d", block),
			fmt.Sprintf("%d", count), "txns",
			percentage,
			strings.Repeat("*", stars)+" "+blockTimeStr,
		)
	}
}
