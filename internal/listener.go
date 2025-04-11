package internal

import (
	"context"
	"fmt"
	"github.com/gagliardetto/solana-go"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
)

// WebsocketListener listens for Solana transaction logs via websockets.
type WebsocketListener struct {
	Subscription *ws.LogSubscription
	Listening    bool
	wsClient     *ws.Client
	reconnectCh  chan struct{}
}

// Start initiates the websocket connection, sets up reconnection logic,
// starts sending transactions, and begins reading log events.
func (l *WebsocketListener) Start() {
	defer Wg.Done()

	// Initialize reconnect channel
	l.reconnectCh = make(chan struct{}, 1)

	// Add a message buffer channel with sufficient capacity
	messageCh := make(chan *ws.LogResult, 1000) // Buffer up to 1000 messages

	// Initial connection
	if GlobalConfig.Debug {
		SimpleLogger.Printf("Attempting to connect to websocket at %s...", GlobalConfig.GetWsUrl())
	} else {
		SimpleLogger.Printf("Connecting to Solana node...")
	}

	if err := l.connect(); err != nil {
		SimpleLogger.Fatalf("error connecting to websocket: %v", err)
	}

	// Set up automatic reconnection handling
	go l.handleReconnection()

	// Start transaction sending after connection is established
	if GlobalConfig.Debug {
		SimpleLogger.Printf("Connected to the websocket. Listening for transactions...")
	} else {
		SimpleLogger.Printf("Connected to Solana network. Ready to start benchmark.")
	}

	// Start transaction sending in a separate goroutine
	go SendTransactions()

	// Start a watchdog to handle "force-stop" after all transactions have been sent
	go l.startWatchdog()

	// Start a separate goroutine to read from subscription and feed messageCh
	go func() {
		for l.Listening {
			if l.Subscription == nil {
				DebugLog("Subscription is nil, waiting...")
				time.Sleep(100 * time.Millisecond)
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			got, err := l.Subscription.Recv(ctx)
			cancel()

			if err != nil {
				// If the error is due to timeout, just continue looping
				if err == context.DeadlineExceeded {
					continue
				}
				if GlobalConfig.Debug {
					SimpleLogger.Printf("Error receiving message: %v", err)
				}
				l.triggerReconnect()
				continue
			}

			if got != nil {
				// Quick pre-check to reduce channel pressure
				if strings.Contains(strings.Join(got.Value.Logs, "\n"), "thorBench") {
					select {
					case messageCh <- got:
						// Message sent to channel
						DebugLog("Queued potential thorBench message: %s", got.Value.Signature.String())
					default:
						// Channel full, log a warning
						if GlobalConfig.Debug {
							SimpleLogger.Printf("Warning: message buffer full, dropping message: %s", got.Value.Signature.String())
						}
					}
				}
			}
		}
		// Close the message channel when listener stops
		close(messageCh)
		DebugLog("Message producer goroutine stopped")
	}()

	// Prepare color helpers
	green := color.New(color.FgGreen).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()

	// Track when to show status updates
	lastStatusTime := time.Now()

	// Main processing loop for subscription events
	l.Listening = true
	if GlobalConfig.Debug {
		SimpleLogger.Printf("Starting main websocket event loop...")
	}

	re := regexp.MustCompile(`(?i)thorBench:Test(\d+)\[(.*?)\]`)
	reWarmup := regexp.MustCompile(`(?i)thorBench:Warmup\[(.*?)\]`)

	for l.Listening {
		select {
		case <-l.reconnectCh:
			// Attempt to reconnect
			if GlobalConfig.Debug {
				SimpleLogger.Printf("Reconnection signal received, attempting to reconnect...")
			} else {
				SimpleLogger.Printf("Network connection issue detected, reconnecting...")
			}

			if err := l.connect(); err != nil {
				if GlobalConfig.Debug {
					SimpleLogger.Printf("Failed to reconnect: %v", err)
				} else {
					SimpleLogger.Printf("Reconnection failed, retrying...")
				}
				// Try again after a short delay
				time.Sleep(1 * time.Second)
				l.triggerReconnect()
			}

		case got, ok := <-messageCh:
			// Check if channel is closed
			if !ok {
				DebugLog("Message channel closed, exiting event loop")
				return
			}

			if got == nil {
				continue
			}

			sig := got.Value.Signature
			logStr := strings.Join(got.Value.Logs, "\n")

			DebugLog("Processing potential test transaction: %s", sig.String())

			// Check if this transaction is one of ours
			Mu.RLock()
			_, isOursInMap := TxTimes[sig]
			isWarmup := (sig == WarmupSignature)
			Mu.RUnlock()

			// If not in our map, check if it matches our patterns
			matches := re.FindStringSubmatch(logStr)
			warmupMatches := reWarmup.FindStringSubmatch(logStr)

			isOurTest := false
			if len(matches) >= 3 && matches[2] == TestID {
				isOurTest = true
				DebugLog("Found test transaction: %s with TestNum: %s and TestID: %s",
					sig.String(), matches[1], matches[2])
			} else if len(matches) >= 3 {
				DebugLog("Transaction has thorBench pattern but different TestID. Got: %s, Expected: %s",
					matches[2], TestID)
			}

			isOurWarmup := false
			if len(warmupMatches) >= 2 && warmupMatches[1] == TestID {
				isOurWarmup = true
				isWarmup = true
				DebugLog("Found warmup transaction: %s", sig.String())
			}

			// Skip if it's not our transaction
			if !isOursInMap && !isWarmup && !isOurTest && !isOurWarmup {
				DebugLog("Transaction doesn't match our TestID: %s", sig.String())
				continue
			}

			// Extract test number if available
			testNum := ""
			txType := "Test"
			if isWarmup || isOurWarmup {
				txType = "Warmup"
			} else if len(matches) >= 2 {
				testNum = matches[1] // Extract test number
			}

			// Store the block time if we don't have it yet
			landBlockHeight := got.Context.Slot
			landTime := time.Now()

			Mu.Lock()
			// Record the transaction landing block height
			if !isWarmup && !isOurWarmup {
				TxLandBlockHeights[sig] = landBlockHeight
			}

			// Record block time if we haven't seen this block yet
			if _, exists := BlockTimes[landBlockHeight]; !exists {
				BlockTimes[landBlockHeight] = landTime
			}
			Mu.Unlock()

			// Determine transaction status
			status := ""
			if strings.Contains(strings.ToLower(logStr), "slippage limit") {
				status = "SLIPPAGE"
				if !isWarmup && !isOurWarmup {
					Mu.Lock()
					SlippageFailures++
					Mu.Unlock()
				}
			} else if got.Value.Err != nil || strings.Contains(logStr, "Error:") {
				status = "FAILED"
				if !isWarmup && !isOurWarmup {
					Mu.Lock()
					OtherFailures++
					Mu.Unlock()
				}
			} else {
				status = "SUCCESS"
				if !isWarmup && !isOurWarmup {
					Mu.Lock()
					ProcessedTransactions++
					Mu.Unlock()
				}
			}

			// Colorize status for output
			var coloredStatus string
			switch status {
			case "SUCCESS":
				coloredStatus = green(status)
			case "SLIPPAGE":
				coloredStatus = yellow(status)
			case "FAILED":
				coloredStatus = red(status)
			default:
				coloredStatus = status
			}

			// Calculate timing and update stats
			var delta time.Duration
			var txSendTime time.Time
			var found bool

			Mu.Lock()
			if isWarmup || isOurWarmup {
				txSendTime = time.Now()
				found = true
				// If this is a warmup but we don't have it stored, save the signature
				if isOurWarmup && WarmupSignature == (solana.Signature{}) {
					WarmupSignature = sig
				}
			} else {
				txSendTime, found = TxTimes[sig]
				if found {
					delta = time.Since(txSendTime)

					// Find the closest block height record for this transaction
					var sendBlockHeight uint64
					minTimeDiff := time.Duration(1<<63 - 1) // Max time.Duration

					for _, record := range BlockHeightHistory {
						if record.TxSig == sig {
							// Exact match for this signature
							sendBlockHeight = record.Height
							break
						}

						// Otherwise find the closest record by time
						timeDiff := txSendTime.Sub(record.Timestamp)
						if timeDiff < 0 {
							timeDiff = -timeDiff
						}

						if timeDiff < minTimeDiff {
							minTimeDiff = timeDiff
							sendBlockHeight = record.Height
						}
					}

					// Calculate block height difference if we have a valid send height
					blockHeightDiff := uint64(0)
					if sendBlockHeight > 0 {
						if landBlockHeight >= sendBlockHeight {
							blockHeightDiff = landBlockHeight - sendBlockHeight
						}
					}

					// Store the landing block height
					TxLandBlockHeights[sig] = landBlockHeight

					// Record in transaction statistics
					TxStats = append(TxStats, TxStat{
						Delta:           delta,
						Status:          status,
						SendBlockHeight: sendBlockHeight,
						LandBlockHeight: landBlockHeight,
						BlockHeightDiff: blockHeightDiff,
						BlockTime:       calculateBlockTime(blockHeightDiff),
					})

					// Track block distribution for successful transactions
					if status == "SUCCESS" {
						if _, ok := TxBlocks[landBlockHeight]; !ok {
							TxBlocks[landBlockHeight] = 0
						}
						TxBlocks[landBlockHeight]++
					}
				} else {
					if GlobalConfig.Debug {
						SimpleLogger.Printf("Warning: Received transaction %s not found in our sent map", sig.String())
					}
				}
			}

			// Get current totals for reporting
			totalLanded := ProcessedTransactions + SlippageFailures + OtherFailures
			Mu.Unlock()

			// Log transaction status
			if found || isOurTest || isOurWarmup {
				if isWarmup || isOurWarmup {
					if GlobalConfig.Debug {
						SimpleLogger.Print(txType+" Tx "+coloredStatus,
							"sig", sig.String(),
						)
					}
				} else {
					Mu.RLock()
					// Find the send block height for this transaction
					var sendBlockHeight uint64
					for _, record := range BlockHeightHistory {
						if record.TxSig == sig {
							sendBlockHeight = record.Height
							break
						}
					}
					blockHeightDiff := uint64(0)
					if sendBlockHeight > 0 && landBlockHeight >= sendBlockHeight {
						blockHeightDiff = landBlockHeight - sendBlockHeight
					}
					Mu.RUnlock()

					if GlobalConfig.Debug {
						logFields := []interface{}{
							"sig", sig.String(),
							"delta", delta.Truncate(time.Millisecond).String(),
							"processed", fmt.Sprintf("%d/%d", totalLanded, SentTransactions),
							"sendBlock", sendBlockHeight,
							"landBlock", landBlockHeight,
							"blockDiff", blockHeightDiff,
						}
						if testNum != "" {
							logFields = append([]interface{}{"num", testNum}, logFields...)
						}
						SimpleLogger.Print(txType+" Tx "+coloredStatus, logFields...)
					}
				}

				// Check stop condition after each transaction
				checkStopCondition()

				// Report progress on significant milestones
				if !GlobalConfig.Debug && (totalLanded%5 == 0 || totalLanded == 1 || totalLanded == SentTransactions) {
					SimpleLogger.Printf("Progress: %d/%d transactions processed", totalLanded, SentTransactions)
				}
			}

		default:
			// Periodically report status even if no messages are being processed
			if time.Since(lastStatusTime) > 5*time.Second {
				Mu.RLock()
				totalLanded := ProcessedTransactions + SlippageFailures + OtherFailures
				notLanded := uint64(0)
				if SentTransactions > totalLanded {
					notLanded = SentTransactions - totalLanded
				}

				// Only show status updates in debug mode
				if GlobalConfig.Debug {
					SimpleLogger.Printf("Status update: Sent=%d, Processed=%d, Failed=%d, NotLanded=%d",
						SentTransactions, ProcessedTransactions, SlippageFailures+OtherFailures, notLanded)
				}
				Mu.RUnlock()
				lastStatusTime = time.Now()
			}

			// Small sleep to prevent CPU spinning
			time.Sleep(10 * time.Millisecond)
		}
	}

	SimpleLogger.Printf("Stopping listening for log events...")
}

// startWatchdog waits for all transactions to be sent, starts a 30-second timer,
// and either quits early once all transactions are confirmed or force-stops on timeout.
func (l *WebsocketListener) startWatchdog() {
	// Wait for all transactions to be sent
	SimpleLogger.Printf("Watchdog started, waiting for transactions to be sent...")
	waitStart := time.Now()

	for {
		if !l.Listening {
			SimpleLogger.Printf("Watchdog stopping because listener is no longer active")
			return
		}

		Mu.RLock()
		sent := SentTransactions
		total := GlobalConfig.TxCount
		Mu.RUnlock()

		if sent >= total {
			SimpleLogger.Printf("All %d transactions have been sent (waited %s)",
				sent, time.Since(waitStart).Round(time.Millisecond))
			break
		}

		// If nothing gets sent for 60 seconds, force watchdog to start
		if time.Since(waitStart) > 60*time.Second {
			SimpleLogger.Printf("No transactions sent in 60 seconds, forcing watchdog to start")
			break
		}

		time.Sleep(200 * time.Millisecond)
	}

	// Once all are sent, start a 30-second timer
	StopTime = time.Now().Add(45 * time.Second) // Increased timeout to 45 seconds

	SimpleLogger.Printf("%s", color.New(color.FgBlue).SprintFunc()("All transactions sent. Stop time set to 45 seconds from now."))

	// Print current status before waiting
	Mu.RLock()
	totalLanded := ProcessedTransactions + SlippageFailures + OtherFailures
	sent := SentTransactions
	Mu.RUnlock()
	SimpleLogger.Printf("Current status: %d/%d transactions processed", totalLanded, sent)

	lastStatusTime := time.Now()

	// Continuously check if all have landed or if the timeout is reached
	for {
		if !l.Listening {
			SimpleLogger.Printf("Watchdog stopping because listener is no longer active")
			return
		}

		Mu.RLock()
		totalLanded := ProcessedTransactions + SlippageFailures + OtherFailures
		sent := SentTransactions
		Mu.RUnlock()

		// Print status update every few seconds for better visibility
		if time.Since(lastStatusTime) > 5*time.Second {
			SimpleLogger.Printf("Status: %d/%d transactions processed, waiting for %d more",
				totalLanded, sent, sent-totalLanded)
			lastStatusTime = time.Now()
		}

		if totalLanded >= sent && sent > 0 {
			l.gracefulShutdown("All transactions confirmed")
			return
		}
		if time.Now().After(StopTime) {
			l.gracefulShutdown(color.New(color.FgRed).SprintFunc()("Force stopping - timeout reached"))
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// gracefulShutdown performs a final log flush and exits.
func (l *WebsocketListener) gracefulShutdown(reason string) {
	SimpleLogger.Printf("%s\nStarting shutdown sequence...", reason)

	// Log current state before shutdown
	Mu.RLock()
	SimpleLogger.Printf("Final status: Sent=%d, Processed=%d, SlippageFailures=%d, OtherFailures=%d",
		SentTransactions, ProcessedTransactions, SlippageFailures, OtherFailures)
	Mu.RUnlock()

	go func() {
		time.Sleep(2 * time.Second) // Wait for final logs
		l.Stop()
	}()
}

// connect (re)establishes the websocket connection and subscribes to logs.
func (l *WebsocketListener) connect() error {
	// Close existing connection if any
	if l.wsClient != nil {
		DebugLog("Closing existing websocket connection...")
		l.wsClient.Close()
	}

	// Create new connection
	if GlobalConfig.Debug {
		SimpleLogger.Printf("Creating new websocket connection to %s...", GlobalConfig.GetWsUrl())
	}

	wsClient, err := ws.Connect(context.Background(), GlobalConfig.GetWsUrl())
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	l.wsClient = wsClient

	// Subscribe to all logs so that our memo instructions (which include our test wallet as signer) are captured.
	if GlobalConfig.Debug {
		SimpleLogger.Printf("Subscribing to logs...")
	}

	l.Subscription, err = l.wsClient.LogsSubscribe(
		"all",
		rpc.CommitmentProcessed,
	)
	if err != nil {
		l.wsClient.Close()
		return fmt.Errorf("failed to subscribe: %v", err)
	}

	if GlobalConfig.Debug {
		SimpleLogger.Printf("Successfully subscribed to logs")
	}

	return nil
}

// handleReconnection tries to reconnect when the channel is signaled.
func (l *WebsocketListener) handleReconnection() {
	DebugLog("Reconnection handler started")
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second
	retryCount := 0

	for range l.reconnectCh {
		if !l.Listening {
			break
		}

		retryCount++
		SimpleLogger.Printf("Reconnection attempt #%d triggered with %s backoff",
			retryCount, backoff)

		if err := l.connect(); err != nil {
			SimpleLogger.Printf("Failed to reconnect (attempt #%d): %v", retryCount, err)

			// Add jitter to backoff (±20% of current backoff)
			jitterRange := int64(float64(backoff) * 0.2)
			jitter := time.Duration(rand.Int63n(jitterRange*2) - jitterRange)
			actualBackoff := backoff + jitter
			if actualBackoff < 100*time.Millisecond {
				actualBackoff = 100 * time.Millisecond
			}

			SimpleLogger.Printf("Will retry in %s", actualBackoff)
			time.Sleep(actualBackoff)

			// Increase backoff for next time, with maximum limit
			backoff = time.Duration(float64(backoff) * 1.5)
			if backoff > maxBackoff {
				backoff = maxBackoff
			}

			l.triggerReconnect()
		} else {
			SimpleLogger.Printf("Reconnection successful after %d attempts", retryCount)
			// Reset backoff and retry count on successful connection
			backoff = 1 * time.Second
			retryCount = 0
		}
	}
	DebugLog("Reconnection handler stopped")
}

// triggerReconnect signals that we need to attempt a websocket reconnect.
func (l *WebsocketListener) triggerReconnect() {
	if !l.Listening {
		return
	}

	DebugLog("Triggering reconnect...")
	select {
	case l.reconnectCh <- struct{}{}:
		DebugLog("Reconnect signal sent")
	default:
		// Channel is full, reconnection already pending
		DebugLog("Reconnect signal queue full, already pending")
	}
}

// Stop ends the subscription and closes the websocket connection.
func (l *WebsocketListener) Stop() {
	if !l.Listening {
		DebugLog("Listener already stopped")
		return
	}

	SimpleLogger.Printf("Stopping websocket listener...")
	l.Listening = false

	if l.Subscription != nil {
		SimpleLogger.Printf("Unsubscribing from logs...")
		l.Subscription.Unsubscribe()
	}

	if l.wsClient != nil {
		SimpleLogger.Printf("Closing websocket connection...")
		l.wsClient.Close()
	}

	SimpleLogger.Printf("Closing reconnect channel...")
	close(l.reconnectCh)

	SimpleLogger.Printf("Websocket listener stopped successfully")
}
