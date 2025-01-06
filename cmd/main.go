package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"golang.org/x/time/rate"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fatih/color"

	"thorbench/internal"
)

func main() {
	// Print the ASCII banner
	internal.PrintBanner()

	// Handle CTRL+C
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println()
		internal.SimpleLogger.Print("CTRL+C detected, Force stopping the test")
		fmt.Println()
		if internal.WsListener != nil && internal.WsListener.Listening {
			internal.WsListener.Stop()
		} else {
			os.Exit(0)
		}
	}()

	// Generate a random 4-byte hex for TestID
	randomBytes := make([]byte, 4)
	if _, err := rand.Read(randomBytes); err != nil {
		panic(err)
	}
	internal.TestID = hex.EncodeToString(randomBytes)

	// Set up logger, read config, verify key, etc.
	internal.SetupLogger()
	internal.GlobalConfig = internal.ReadConfig()
	internal.VerifyPrivateKey(internal.GlobalConfig.PrivateKey)

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
	// Ensure we have enough SOL for creating ATAs, paying fees, etc.
	internal.AssertSufficientBalance()

	// Start the websocket listener
	internal.Wg.Add(1)
	internal.WsListener = new(internal.WebsocketListener)
	// Force stop in 1 minute
	internal.StopTime = time.Now().Add(1 * time.Minute)
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

	// Recompute the fee (same as before) for final display
	finalCUCostSOL := (lamportsPerCU*float64(internal.GlobalConfig.ComputeUnitLimit) + 5000) / 1_000_000_000.0
	internal.SimpleLogger.Printf("%s %f Lamports (%.9f SOL)",
		green("Priority Fee/CU        :"),
		lamportsPerCU,
		finalCUCostSOL,
	)
	internal.SimpleLogger.Printf("%s %d", green("Node Retries           :"), internal.GlobalConfig.NodeRetries)

	// Print transaction stats if any were actually sent
	if internal.SentTransactions > 0 {
		internal.PrintStatsAndBlocks()
	}

	fmt.Println()
	fmt.Printf("Benchmark results saved to %s\n", internal.LogFileName)
	fmt.Println("Press 'Enter' to exit...")
	fmt.Scanln()
}
