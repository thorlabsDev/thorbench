package internal

import (
	"context"
	"fmt"
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

	// Initial connection
	if err := l.connect(); err != nil {
		SimpleLogger.Fatalf("error connecting to websocket: %v", err)
	}

	// Set up automatic reconnection handling
	go l.handleReconnection()

	// Start transaction sending after connection is established
	SimpleLogger.Printf("Connected to the websocket. Listening for transactions...")
	SendTransactions()

	// Start a watchdog to handle "force-stop" after all transactions have been sent
	go l.startWatchdog()

	// Prepare color helpers
	green := color.New(color.FgGreen).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()

	// Main processing loop for subscription events
	l.Listening = true
	for l.Listening {
		select {
		case <-l.reconnectCh:
			// Attempt to reconnect
			if err := l.connect(); err != nil {
				SimpleLogger.Printf("Failed to reconnect: %v", err)
				continue
			}

		default:
			if l.Subscription == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			got, err := l.Subscription.Recv(context.Background())
			if err != nil {
				SimpleLogger.Printf("Error receiving message: %v", err)
				l.triggerReconnect()
				continue
			}
			if got == nil {
				continue
			}

			sig := got.Value.Signature
			logStr := strings.Join(got.Value.Logs, "\n")

			// Look for "thorBench:Test" pattern
			re := regexp.MustCompile(`(?i)thorBench:Test(\d+)\[(.*?)\]`)
			matches := re.FindStringSubmatch(logStr)

			// Check if this transaction is one of ours
			Mu.RLock()
			_, isOurs := TxTimes[sig]
			isWarmup := (sig == WarmupSignature)
			Mu.RUnlock()

			// If it's not ours and not warmup, see if it matches our test ID
			if !isOurs && !isWarmup {
				if len(matches) < 3 {
					continue
				}
				if matches[2] != TestID {
					continue
				}
			}

			// Identify transaction type and test number
			testNum := ""
			txType := "Test"
			if isWarmup {
				txType = "Warmup"
			} else if len(matches) >= 2 {
				testNum = matches[1] // Extract test number
			}

			// Determine transaction status
			status := ""
			if strings.Contains(strings.ToLower(logStr), "slippage limit") {
				status = "SLIPPAGE"
				if !isWarmup {
					Mu.Lock()
					SlippageFailures++
					Mu.Unlock()
				}
			} else if got.Value.Err != nil || strings.Contains(logStr, "Error:") {
				status = "FAILED"
				if !isWarmup {
					Mu.Lock()
					OtherFailures++
					Mu.Unlock()
				}
			} else {
				status = "SUCCESS"
				if !isWarmup {
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
			Mu.Lock()
			var txSendTime time.Time
			var found bool

			if isWarmup {
				txSendTime = time.Now()
				found = true
			} else {
				txSendTime, found = TxTimes[sig]
				if found {
					delta = time.Since(txSendTime)
					TxStats = append(TxStats, TxStat{
						Delta:  delta,
						Status: status,
					})

					// Track block distribution for successful transactions
					if status == "SUCCESS" {
						if _, ok := TxBlocks[got.Context.Slot]; !ok {
							TxBlocks[got.Context.Slot] = 0
						}
						TxBlocks[got.Context.Slot]++
					}
				}
			}
			Mu.Unlock()

			// Log transaction status
			if found {
				if isWarmup {
					SimpleLogger.Print(txType+" Tx "+coloredStatus,
						"sig", sig.String(),
					)
				} else {
					Mu.RLock()
					totalLanded := ProcessedTransactions + SlippageFailures + OtherFailures
					Mu.RUnlock()
					logFields := []interface{}{
						"sig", sig.String(),
						"delta", delta.Truncate(time.Millisecond).String(),
						"processed",
						fmt.Sprintf("%d/%d", totalLanded, SentTransactions),
					}
					if testNum != "" {
						logFields = append([]interface{}{"num", testNum}, logFields...)
					}
					SimpleLogger.Print(txType+" Tx "+coloredStatus, logFields...)
				}
			}
		}
	}

	SimpleLogger.Printf("Stopping listening for log events...")
}

// startWatchdog waits for all transactions to be sent, starts a 30-second timer,
// and either quits early once all transactions are confirmed or force-stops on timeout.
func (l *WebsocketListener) startWatchdog() {
	// Wait for all transactions to be sent
	for {
		Mu.RLock()
		sent := SentTransactions
		total := GlobalConfig.TxCount
		Mu.RUnlock()

		if sent >= total {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Once all are sent, start a 30-second timer
	StopTime = time.Now().Add(30 * time.Second)

	SimpleLogger.Printf("%s", color.New(color.FgBlue).SprintFunc()("All transactions sent. Stop time set to 30 seconds from now."))

	// Continuously check if all have landed or if the timeout is reached
	for {
		Mu.RLock()
		totalLanded := ProcessedTransactions + SlippageFailures + OtherFailures
		sent := SentTransactions
		Mu.RUnlock()

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
	go func() {
		time.Sleep(2 * time.Second) // Wait for final logs
		l.Stop()
	}()
}

// connect (re)establishes the websocket connection and subscribes to logs.
func (l *WebsocketListener) connect() error {
	// Close existing connection if any
	if l.wsClient != nil {
		l.wsClient.Close()
	}

	// Create new connection
	wsClient, err := ws.Connect(context.Background(), GlobalConfig.GetWsUrl())
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	l.wsClient = wsClient

	// Subscribe to logs
	l.Subscription, err = l.wsClient.LogsSubscribeMentions(
		TestAccount.PublicKey(),
		rpc.CommitmentProcessed,
	)
	if err != nil {
		l.wsClient.Close()
		return fmt.Errorf("failed to subscribe: %v", err)
	}

	return nil
}

// handleReconnection tries to reconnect when the channel is signaled.
func (l *WebsocketListener) handleReconnection() {
	for range l.reconnectCh {
		if err := l.connect(); err != nil {
			SimpleLogger.Printf("Failed to reconnect: %v", err)
			// Try again after a short delay
			time.Sleep(1 * time.Second)
			l.triggerReconnect()
		}
	}
}

// triggerReconnect signals that we need to attempt a websocket reconnect.
func (l *WebsocketListener) triggerReconnect() {
	select {
	case l.reconnectCh <- struct{}{}:
	default:
		// Channel is full, reconnection already pending
	}
}

// Stop ends the subscription and closes the websocket connection.
func (l *WebsocketListener) Stop() {
	if !l.Listening {
		return
	}
	l.Listening = false
	if l.Subscription != nil {
		l.Subscription.Unsubscribe()
	}
	if l.wsClient != nil {
		l.wsClient.Close()
	}
	close(l.reconnectCh)
}
