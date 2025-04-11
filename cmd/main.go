package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fatih/color"
	"golang.org/x/time/rate"

	"thorbench/internal"
)

func main() {
	// Get executable directory and change to it
	ex, err := os.Executable()
	if err != nil {
		fmt.Printf("Error getting executable path: %v\n", err)
		os.Exit(1)
	}
	execDir := filepath.Dir(ex)
	if err := os.Chdir(execDir); err != nil {
		fmt.Printf("Error changing to executable directory: %v\n", err)
		os.Exit(1)
	}

	// Print the ASCII banner
	internal.PrintBanner()

	// Handle CTRL+C
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println()
		if internal.SimpleLogger != nil {
			internal.SimpleLogger.Print("CTRL+C detected, Force stopping the test")
			internal.FlushLogs() // Make sure we flush logs
		} else {
			fmt.Println("CTRL+C detected, Force stopping the test")
		}
		fmt.Println()
		if internal.WsListener != nil && internal.WsListener.Listening {
			internal.WsListener.Stop()
		} else {
			internal.CleanupLogger() // Make sure we properly close log files
			os.Exit(0)
		}
	}()

	// Generate a random 4-byte hex for TestID
	randomBytes := make([]byte, 4)
	if _, err := rand.Read(randomBytes); err != nil {
		panic(err)
	}
	internal.TestID = hex.EncodeToString(randomBytes)

	// Set up logger FIRST, before any config operations
	internal.SetupLogger()

	// Ensure we cleanup logs at exit
	defer internal.CleanupLogger()

	// Now read config and perform other initialization
	internal.GlobalConfig = internal.ReadConfig()

	// Validate and verify private key
	fmt.Println("Verifying private key...")
	internal.VerifyPrivateKey(internal.GlobalConfig.PrivateKey)
	fmt.Println("Private key verified successfully")

	// Configure the rate limiter
	internal.Limiter.SetLimit(rate.Limit(internal.GlobalConfig.RateLimit))
	internal.Limiter.SetBurst(int(internal.GlobalConfig.RateLimit))

	// Prepare color helpers
	green := color.New(color.FgGreen).SprintFunc()

	// Print some test info
	nowUTC := time.Now().UTC().Format(time.RFC1123)
	fmt.Println()
	internal.SimpleLogger.Printf("%s %s", green("Date                   :"), nowUTC)
	internal.SimpleLogger.Printf("%s %s", green("Test Wallet            :"), internal.TestAccount.PublicKey().String())
	internal.SimpleLogger.Printf("%s %s", green("Starting Test ID       :"), internal.TestID)
	internal.SimpleLogger.Printf("%s %s", green("RPC URL                :"), internal.GlobalConfig.RpcUrl)
	internal.SimpleLogger.Printf("%s %s", green("WS URL                 :"), internal.GlobalConfig.GetWsUrl())
	internal.SimpleLogger.Printf("%s %s", green("RPC Send URL           :"), internal.GlobalConfig.GetSendUrl())
	internal.SimpleLogger.Printf("%s %d", green("Rate Limit             :"), internal.GlobalConfig.RateLimit)
	internal.SimpleLogger.Printf("%s %d", green("Transaction Count      :"), internal.GlobalConfig.TxCount)
	internal.SimpleLogger.Printf("%s %.1f", green("Slippage               :"), internal.GlobalConfig.Slippage)
	internal.SimpleLogger.Printf("%s %.4f", green("Token Swap Amount      :"), internal.GlobalConfig.Amount)
	internal.SimpleLogger.Printf("%s %d", green("Compute Unit Limit     :"), internal.GlobalConfig.ComputeUnitLimit)
	internal.SimpleLogger.Printf("%s %v", green("Debug Mode             :"), internal.GlobalConfig.Debug)

	// Show user the Priority Fee in SOL terms
	lamportsPerCU := internal.GlobalConfig.PrioFee
	totalCUCostLamports := lamportsPerCU*float64(internal.GlobalConfig.ComputeUnitLimit) + 5000
	totalCUCostSOL := totalCUCostLamports / 1_000_000_000.0

	internal.SimpleLogger.Printf("%s %f Lamports (%.9f SOL)",
		green("Priority Fee/CU        :"),
		lamportsPerCU,
		totalCUCostSOL,
	)
	internal.SimpleLogger.Printf("%s %d", green("Node Retries           :"), internal.GlobalConfig.NodeRetries)
	internal.SimpleLogger.Printf("")
	internal.SimpleLogger.Printf("Initializing...")
	internal.FlushLogs() // Force flush logs here

	// Ensure we have enough SOL for creating ATAs, paying fees, etc.
	internal.SimpleLogger.Printf("Checking wallet balance...")
	internal.AssertSufficientBalance()
	internal.SimpleLogger.Printf("Balance check passed")
	internal.FlushLogs()

	// Add timeout protection for the entire program
	programTimeout := time.AfterFunc(5*time.Minute, func() {
		internal.SimpleLogger.Printf("EMERGENCY TIMEOUT: Program has been running for 5 minutes without completing")
		internal.SimpleLogger.Printf("This is likely due to a deadlock or infinite loop")
		internal.SimpleLogger.Printf("Current state: SentTx=%d, Processed=%d, Slippage=%d, Other=%d",
			internal.SentTransactions, internal.ProcessedTransactions,
			internal.SlippageFailures, internal.OtherFailures)

		internal.FlushLogs()
		os.Exit(1)
	})
	defer programTimeout.Stop()

	// Start the websocket listener
	internal.Wg.Add(1)
	internal.WsListener = new(internal.WebsocketListener)

	go internal.WsListener.Start()

	// Wait until the listener finishes (or is force-stopped)
	internal.Wg.Wait()

	// Summarize final stats
	internal.SimpleLogger.Printf("%s %s", green("Finished Test ID       :"), internal.TestID)
	internal.SimpleLogger.Printf("%s %s", green("Test Wallet            :"), internal.TestAccount.PublicKey().String())
	internal.SimpleLogger.Printf("%s %s", green("RPC URL                :"), internal.GlobalConfig.RpcUrl)
	internal.SimpleLogger.Printf("%s %s", green("WS URL                 :"), internal.GlobalConfig.GetWsUrl())
	internal.SimpleLogger.Printf("%s %s", green("RPC Send URL           :"), internal.GlobalConfig.GetSendUrl())
	internal.SimpleLogger.Printf("%s %d", green("Rate Limit             :"), internal.GlobalConfig.RateLimit)
	internal.SimpleLogger.Printf("%s %d", green("Transaction Count      :"), internal.GlobalConfig.TxCount)
	internal.SimpleLogger.Printf("%s %.1f", green("Slippage               :"), internal.GlobalConfig.Slippage)
	internal.SimpleLogger.Printf("%s %.4f", green("Token Swap Amount      :"), internal.GlobalConfig.Amount)
	internal.SimpleLogger.Printf("%s %d", green("Compute Unit Limit     :"), internal.GlobalConfig.ComputeUnitLimit)

	finalCUCostSOL := (lamportsPerCU*float64(internal.GlobalConfig.ComputeUnitLimit) + 5000) / 1_000_000_000.0
	internal.SimpleLogger.Printf("%s %f Lamports (%.9f SOL)",
		green("Priority Fee/CU        :"),
		lamportsPerCU,
		finalCUCostSOL,
	)
	internal.SimpleLogger.Printf("%s %d", green("Node Retries           :"), internal.GlobalConfig.NodeRetries)

	if internal.SentTransactions > 0 {
		internal.PrintStatsAndBlocks()
	}

	if internal.GlobalConfig.Debug {
		internal.DebugStats.PrintStats()
	}

	fmt.Println()
	fmt.Printf("Benchmark results saved to %s\n", internal.LogFileName)

	// Force flush logs one last time
	internal.FlushLogs()

	fmt.Println("Press 'Enter' to exit...")
	fmt.Scanln()

	// Cleanup logs on exit
	internal.CleanupLogger()
}
