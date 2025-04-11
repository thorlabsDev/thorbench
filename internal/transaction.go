package internal

import (
	"context"
	"fmt"
	"time"

	"github.com/gagliardetto/solana-go"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/itherunder/raydium-swap-go/raydium"
	"github.com/itherunder/raydium-swap-go/raydium/layouts"
	"github.com/itherunder/raydium-swap-go/raydium/trade"
	"github.com/itherunder/raydium-swap-go/raydium/utils"
)

const (
	warmUpMaxRetries = 3
	warmUpRpcRetries = 10 // Increased RPC retries for warmup
)

// Global variable to store warmup TX signature
var WarmupSignature solana.Signature

// Function to fetch pool keys with retries
func fetchPoolKeysWithRetry(raydiumClient *raydium.Raydium, inputToken, outputToken *utils.Token, maxRetries int) (*layouts.ApiPoolInfoV4, error) {
	var poolKeys *layouts.ApiPoolInfoV4
	var lastError error

	if GlobalConfig.Debug {
		SimpleLogger.Printf("Fetching Raydium pool keys... (this may take a moment)")
	} else {
		SimpleLogger.Printf("Preparing to fetch liquidity pool data...")
	}

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if GlobalConfig.Debug {
			SimpleLogger.Printf("Fetching Raydium pool keys (attempt %d/%d)...", attempt, maxRetries)
		} else if attempt > 1 {
			SimpleLogger.Printf("Pool fetch retry %d/%d...", attempt, maxRetries)
		}

		// Create context with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// Try to get pool keys with timeout
		poolKeysChan := make(chan *layouts.ApiPoolInfoV4, 1)
		errChan := make(chan error, 1)

		go func() {
			poolKeys, err := raydiumClient.Pool.GetPoolKeys(inputToken.Mint, outputToken.Mint)
			if err != nil {
				errChan <- err
				return
			}
			poolKeysChan <- poolKeys
		}()

		// Wait for result or timeout
		select {
		case poolKeys = <-poolKeysChan:
			if GlobalConfig.Debug {
				SimpleLogger.Printf("Successfully retrieved pool keys on attempt %d", attempt)
			} else {
				SimpleLogger.Printf("Successfully connected to liquidity pool")
			}
			return poolKeys, nil
		case err := <-errChan:
			lastError = err
			if GlobalConfig.Debug {
				SimpleLogger.Printf("Error getting pool keys (attempt %d): %v", attempt, err)
			}
		case <-ctx.Done():
			lastError = fmt.Errorf("timed out waiting for pool keys")
			if GlobalConfig.Debug {
				SimpleLogger.Printf("Timed out getting pool keys (attempt %d)", attempt)
			}
		}

		// If this wasn't the last attempt, wait before retrying
		if attempt < maxRetries {
			retryDelay := time.Duration(attempt) * 2 * time.Second
			if GlobalConfig.Debug {
				SimpleLogger.Printf("Retrying in %s...", retryDelay)
			}
			time.Sleep(retryDelay)
		}
	}

	return nil, fmt.Errorf("failed to get pool keys after %d attempts: %v", maxRetries, lastError)
}

// SendTransaction sends a single transaction and properly tracks its send time
func SendTransaction(id uint64, rpcClient *rpc.Client, raydiumClient *raydium.Raydium,
	user solana.PublicKey, poolKeys *layouts.ApiPoolInfoV4,
	inputToken *utils.Token, outputToken *utils.Token, slippage *utils.Percent) {

	// Use Rate limiter
	if err := Limiter.Wait(context.TODO()); err != nil {
		DebugLog("Thread %d: Rate limiter error: %v", id, err)
		Mu.Lock()
		OtherFailures++
		SentTransactions++
		Mu.Unlock()
		checkStopCondition()
		return
	}

	// Query amounts
	DebugLog("Thread %d: Querying amounts...", id)
	amount := utils.NewTokenAmount(inputToken, GlobalConfig.Amount)
	amountsOut, err := raydiumClient.Liquidity.GetAmountsOut(poolKeys, amount, slippage)
	if err != nil {
		if GlobalConfig.Debug {
			SimpleLogger.Printf("Thread %d: Error getting amounts out: %v", id, err)
		}
		Mu.Lock()
		OtherFailures++
		SentTransactions++
		Mu.Unlock()
		checkStopCondition()
		return
	}
	DebugLog("Thread %d: Amount in: %.0f, Min amount out: %.0f", id, float64(amountsOut.AmountIn.Amount), float64(amountsOut.MinAmountOut.Amount))

	// Build swap
	DebugLog("Thread %d: Building swap transaction...", id)
	swapTx, err := raydiumClient.Trade.MakeSwapTransaction(
		poolKeys,
		amountsOut.AmountIn,
		amountsOut.MinAmountOut,
		trade.FeeConfig{
			MicroLamports: uint64(GlobalConfig.PrioFee * 1e6),
		},
	)
	if err != nil {
		if GlobalConfig.Debug {
			SimpleLogger.Printf("Thread %d: Error making swap transaction: %v", id, err)
		}
		Mu.Lock()
		OtherFailures++
		SentTransactions++
		Mu.Unlock()
		checkStopCondition()
		return
	}

	// Extract Raydium instructions
	raydiumInstrs := compiledToInstructions(
		swapTx.Message.Instructions,
		swapTx.Message.AccountKeys,
	)

	// Filter out any compute budget instructions
	var filteredInstrs []solana.Instruction
	computeBudgetProgram := solana.MustPublicKeyFromBase58("ComputeBudget111111111111111111111111111111")

	for _, instr := range raydiumInstrs {
		if !instr.ProgramID().Equals(computeBudgetProgram) {
			filteredInstrs = append(filteredInstrs, instr)
		} else {
			DebugLog("Thread %d: Removing existing compute budget instruction", id)
		}
	}

	// Add our compute budget instructions
	var allInstrs []solana.Instruction
	allInstrs = append(allInstrs,
		computebudget.NewSetComputeUnitLimitInstruction(uint32(GlobalConfig.ComputeUnitLimit)).Build(),
		computebudget.NewSetComputeUnitPriceInstruction(uint64(GlobalConfig.PrioFee*1e6)).Build(),
	)

	// Add memo instruction
	memoData := createCleanMemo("thorBench:Test%d[%s]", id, TestID)
	memoInst := createRawMemoInstruction(memoData, user)
	allInstrs = append(allInstrs, memoInst)

	// Add the filtered Raydium instructions
	allInstrs = append(allInstrs, filteredInstrs...)

	DebugLog("Thread %d: Getting latest blockhash...", id)
	latest, err := rpcClient.GetLatestBlockhash(context.TODO(), rpc.CommitmentFinalized)
	if err != nil {
		if GlobalConfig.Debug {
			SimpleLogger.Printf("Thread %d: Error getting latest blockhash: %v", id, err)
		}
		Mu.Lock()
		OtherFailures++
		SentTransactions++
		Mu.Unlock()
		checkStopCondition()
		return
	}

	DebugLog("Thread %d: Creating transaction...", id)
	finalTx, err := solana.NewTransaction(
		allInstrs,
		latest.Value.Blockhash,
		solana.TransactionPayer(user),
	)
	if err != nil {
		if GlobalConfig.Debug {
			SimpleLogger.Printf("Thread %d: Error creating transaction: %v", id, err)
		}
		Mu.Lock()
		OtherFailures++
		SentTransactions++
		Mu.Unlock()
		checkStopCondition()
		return
	}

	DebugLog("Thread %d: Signing transaction...", id)
	_, err = finalTx.Sign(func(pk solana.PublicKey) *solana.PrivateKey {
		if pk.Equals(user) {
			return TestAccount
		}
		return nil
	})
	if err != nil {
		if GlobalConfig.Debug {
			SimpleLogger.Printf("Thread %d: Error signing transaction: %v", id, err)
		}
		Mu.Lock()
		OtherFailures++
		SentTransactions++
		Mu.Unlock()
		checkStopCondition()
		return
	}

	// IMPORTANT: Record time BEFORE sending the transaction
	txSendTime := time.Now()

	// Start a goroutine to get the current block height in parallel
	getBlockHeightChan := make(chan uint64, 1)
	go func() {
		// Create a context with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Get the current slot
		slot, err := rpcClient.GetSlot(ctx, rpc.CommitmentProcessed)
		if err != nil {
			DebugLog("Thread %d: Error getting current slot: %v", id, err)
			close(getBlockHeightChan)
			return
		}

		getBlockHeightChan <- slot
		close(getBlockHeightChan)
	}()

	DebugLog("Thread %d: Sending transaction...", id)
	sig, err := rpcClient.SendTransactionWithOpts(
		context.TODO(),
		finalTx,
		rpc.TransactionOpts{
			SkipPreflight: true,
			MaxRetries:    &GlobalConfig.NodeRetries,
		},
	)

	// Record transaction info regardless of send success/failure
	Mu.Lock()
	SentTransactions++
	if err == nil {
		// Only store signature timing if send was successful
		TxTimes[sig] = txSendTime // Use the time from before sending

		// Attempt to get block height from parallel request (with short timeout)
		select {
		case blockHeight, ok := <-getBlockHeightChan:
			if ok {
				// Record the current block height with timestamp and transaction signature
				record := BlockHeightRecord{
					Height:    blockHeight,
					Timestamp: txSendTime,
					TxSig:     sig,
				}
				BlockHeightHistory = append(BlockHeightHistory, record)

				if GlobalConfig.Debug {
					SimpleLogger.Print("Tx SENT",
						"threadID", id,
						"sig", sig.String(),
						"blockHeight", blockHeight,
						"time", txSendTime.Format("15:04:05.000"),
					)
				}
			} else {
				if GlobalConfig.Debug {
					SimpleLogger.Print("Tx SENT",
						"threadID", id,
						"sig", sig.String(),
						"time", txSendTime.Format("15:04:05.000"),
					)
				}
			}
		case <-time.After(500 * time.Millisecond):
			// Timeout waiting for block height, proceed without it
			if GlobalConfig.Debug {
				SimpleLogger.Print("Tx SENT",
					"threadID", id,
					"sig", sig.String(),
					"time", txSendTime.Format("15:04:05.000"),
				)
			}
		}
	} else {
		if GlobalConfig.Debug {
			SimpleLogger.Printf("Thread %d: Error sending transaction: %v", id, err)
		}
		OtherFailures++
	}
	Mu.Unlock()

	// Drain the channel if we didn't use it
	go func() {
		for range getBlockHeightChan {
			// Drain the channel
		}
	}()

	checkStopCondition()
}

// SendTransactions prepares and sends the Raydium swap transactions
func SendTransactions() {
	if GlobalConfig.Debug {
		SimpleLogger.Printf("Starting SendTransactions()...")
	} else {
		SimpleLogger.Printf("Initializing transaction pipeline...")
	}

	rpcClient := InitializeRPCClient(GlobalConfig.GetSendUrl())
	DebugLog("RPC client initialized with URL: %s", GlobalConfig.GetSendUrl())

	raydiumClient := raydium.New(rpcClient, GlobalConfig.PrivateKey)
	DebugLog("Raydium client initialized")

	user := TestAccount.PublicKey()
	DebugLog("Using wallet: %s", user.String())

	// 1) Decide which mints to trade
	inMint := GlobalConfig.InputMint
	outMint := GlobalConfig.OutputMint
	if inMint == "" {
		inMint = SOLMint.String()
	}
	if outMint == "" {
		outMint = RAYMint.String()
	}
	SimpleLogger.Printf("Trading from %s to %s", inMint, outMint)

	inMintPK := solana.MustPublicKeyFromBase58(inMint)
	outMintPK := solana.MustPublicKeyFromBase58(outMint)

	// 2) Fetch decimals
	DebugLog("Fetching decimals for input mint %s...", inMint)
	inDecimals, err := FetchDecimalsForMint(rpcClient, inMintPK)
	if err != nil {
		SimpleLogger.Fatalf("error fetching decimals for input mint %s: %v", inMint, err)
	}

	if GlobalConfig.Debug {
		SimpleLogger.Printf("Input mint %s has %d decimals", inMint, inDecimals)
	}

	DebugLog("Fetching decimals for output mint %s...", outMint)
	outDecimals, err := FetchDecimalsForMint(rpcClient, outMintPK)
	if err != nil {
		SimpleLogger.Fatalf("error fetching decimals for output mint %s: %v", outMint, err)
	}

	if GlobalConfig.Debug {
		SimpleLogger.Printf("Output mint %s has %d decimals", outMint, outDecimals)
	}

	inputToken := utils.NewToken("INPUT", inMint, uint64(inDecimals))
	outputToken := utils.NewToken("OUTPUT", outMint, uint64(outDecimals))

	// 3) Get Raydium pool keys with retries
	poolKeys, err := fetchPoolKeysWithRetry(raydiumClient, inputToken, outputToken, 3)
	if err != nil {
		// Suggest alternatives when pool keys can't be found
		SimpleLogger.Printf("Failed to find a pool for %s <-> %s: %v", inMint, outMint, err)
		SimpleLogger.Printf("This typically means either:")
		SimpleLogger.Printf("1. The token pair does not have a Raydium liquidity pool")
		SimpleLogger.Printf("2. The RPC node doesn't have the pool information")
		SimpleLogger.Printf("\nTry these alternatives:")
		SimpleLogger.Printf("- Use SOL/USDC pair: So11111111111111111111111111111111111111112 and EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
		SimpleLogger.Printf("- Use SOL/USDT pair: So11111111111111111111111111111111111111112 and Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
		SimpleLogger.Printf("- Check your network health")
		SimpleLogger.Printf("- Use a different RPC provider")
		SimpleLogger.Fatalf("Exiting due to pool key retrieval failure")
	}

	// 4) Convert config slippage
	slipValue := int64(GlobalConfig.Slippage * 100)
	slippage := utils.NewPercent(uint64(slipValue), 10000)
	DebugLog("Using slippage: %d/%d", slippage.Numerator, slippage.Denominator)

	// 5) Do warm-up TX if not skipped
	if !GlobalConfig.SkipWarmup {
		SimpleLogger.Printf("Starting warm-up transaction...")
		warmUpSuccess := false
		var lastError error

		for attempt := 1; attempt <= warmUpMaxRetries; attempt++ {
			DebugLog("Warm-up attempt %d of %d", attempt, warmUpMaxRetries)
			err := doWarmUpTx(
				rpcClient,
				raydiumClient,
				user,
				poolKeys,
				inputToken,
				outputToken,
				slippage,
			)
			if err == nil {
				SimpleLogger.Printf("Warm-up TX succeeded on attempt %d", attempt)
				warmUpSuccess = true

				// Wait for the warm-up TX to finalize before starting benchmark
				SimpleLogger.Printf("Waiting for warm-up TX to finalize...")
				time.Sleep(5 * time.Second) // Initial delay to let TX propagate

				// Poll for finalization
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()

				minConfirmations := uint64(32)
				minAcceptableConfirmations := uint64(20)
				var lastConfirmations uint64

				for {
					select {
					case <-ctx.Done():
						if lastConfirmations >= minAcceptableConfirmations {
							SimpleLogger.Printf("Proceeding with benchmark after %d confirmations (ideal: >32)", lastConfirmations)
							goto START_BENCHMARK
						}
						SimpleLogger.Fatal("Timed out waiting for warm-up TX to finalize")
					default:
						status, err := rpcClient.GetSignatureStatuses(
							context.Background(),
							true,            // searchTransactionHistory
							WarmupSignature, // Pass the signature directly, not as string
						)
						if err != nil {
							DebugLog("Error checking warmup signature status: %v, retrying...", err)
							time.Sleep(2 * time.Second)
							continue
						}

						if status == nil {
							DebugLog("Got nil status response, retrying...")
							time.Sleep(2 * time.Second)
							continue
						}

						if len(status.Value) == 0 || status.Value[0] == nil {
							DebugLog("Transaction not found in ledger yet, retrying...")
							time.Sleep(2 * time.Second)
							continue
						}

						txStatus := status.Value[0]
						if txStatus.Err != nil {
							SimpleLogger.Printf("Warm-up transaction failed with error: %v", txStatus.Err)
							goto START_BENCHMARK // Still proceed with benchmark despite error
						}

						// Check if transaction is already finalized (nil confirmations means finalized)
						if txStatus.Confirmations == nil && txStatus.Err == nil {
							SimpleLogger.Printf("Warm-up TX is finalized. Starting benchmark...")
							goto START_BENCHMARK
						}

						// Otherwise check confirmation count
						if txStatus.Confirmations != nil {
							confirmations := *txStatus.Confirmations
							lastConfirmations = confirmations
							if confirmations > minConfirmations {
								SimpleLogger.Printf("Warm-up TX finalized with %d confirmations. Starting benchmark...", confirmations)
								goto START_BENCHMARK
							} else {
								DebugLog("Warm-up TX has %d confirmations, waiting for >%d...", confirmations, minConfirmations)
							}
						}
						time.Sleep(2 * time.Second)
					}
				}
			}
			lastError = err
			SimpleLogger.Printf("Warm-up TX attempt %d failed: %v", attempt, err)
			time.Sleep(2 * time.Second)
		}
		if !warmUpSuccess {
			SimpleLogger.Printf("Warm-up TX failed after all retries. Last error: %v", lastError)
			SimpleLogger.Printf("Proceeding with benchmark anyway...")
			// Continue anyway, don't abort
		}
	} else {
		SimpleLogger.Printf("Skipping warm-up transaction as requested")
	}

START_BENCHMARK:
	// 6) Run concurrent transactions
	SimpleLogger.Printf("Starting benchmark transactions...")

	// In debug mode start immediately rather than waiting 10 seconds.
	startTime := time.Now().Truncate(5 * time.Second).Add(10 * time.Second)
	if GlobalConfig.Debug {
		startTime = time.Now()
	}

	for i := uint64(0); i < GlobalConfig.TxCount; i++ {
		go func(id uint64) {
			sleepTime := time.Until(startTime)
			if id == 1 && GlobalConfig.Debug {
				SimpleLogger.Print("Threads sleeping until benchmark start", "delay", sleepTime.Truncate(time.Millisecond))
			}
			time.Sleep(sleepTime)

			// Create a new RPC client for each goroutine if in debug mode
			var txRpcClient *rpc.Client
			if GlobalConfig.Debug {
				txRpcClient = InitializeRPCClient(GlobalConfig.GetSendUrl())
				raydiumClient = raydium.New(txRpcClient, GlobalConfig.PrivateKey)
			} else {
				txRpcClient = rpcClient
			}

			// Use enhanced transaction sending function
			SendTransaction(id, txRpcClient, raydiumClient, user, poolKeys, inputToken, outputToken, slippage)
		}(i + 1)
	}
}

// ---------------------------------------
// WARM-UP TX
// ---------------------------------------
func doWarmUpTx(
	rpcClient *rpc.Client,
	raydiumClient *raydium.Raydium,
	user solana.PublicKey,
	poolKeys *layouts.ApiPoolInfoV4,
	inputToken *utils.Token,
	outputToken *utils.Token,
	slippage *utils.Percent,
) error {
	const (
		maxSendRetries = 3
		sendTimeout    = 30 * time.Second
		confirmTimeout = 45 * time.Second
	)

	var sig solana.Signature
	var err error
	var sendAttempt int

	// Create a debug client if debug mode is enabled
	if GlobalConfig.Debug {
		DebugLog("Using debug client for warm-up transaction")
		rpcClient = InitializeRPCClient(GlobalConfig.GetSendUrl())
		raydiumClient = raydium.New(rpcClient, GlobalConfig.PrivateKey)
	}

	// Keep trying to send until we get confirmation or hit max retries
	for sendAttempt = 1; sendAttempt <= maxSendRetries; sendAttempt++ {
		DebugLog("Warm-up send attempt %d of %d", sendAttempt, maxSendRetries)

		// Create and send transaction with timeout
		sendCtx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		sig, err = sendWarmupTransaction(sendCtx, rpcClient, raydiumClient, user, poolKeys, inputToken, outputToken, slippage)
		cancel()

		if err != nil {
			if sendAttempt < maxSendRetries {
				SimpleLogger.Printf("Warm-up TX send attempt %d failed: %v, retrying...", sendAttempt, err)
				time.Sleep(2 * time.Second)
				continue
			}
			return fmt.Errorf("all warm-up TX send attempts failed: %w", err)
		}

		SimpleLogger.Printf("Warm-up transaction sent, waiting for confirmation, signature: %s", sig.String())

		// Wait for the transaction to be confirmed
		confirmCtx, cancel := context.WithTimeout(context.Background(), confirmTimeout)
		confirmed, err := waitForTxConfirmation(confirmCtx, rpcClient, sig)
		cancel()

		if err != nil {
			DebugLog("Error waiting for confirmation: %v", err)
			if sendAttempt < maxSendRetries {
				SimpleLogger.Printf("Warm-up TX confirmation attempt %d failed, retrying send...", sendAttempt)
				time.Sleep(2 * time.Second)
				continue
			}
			return fmt.Errorf("all warm-up TX confirmation attempts failed: %w", err)
		}

		if !confirmed {
			DebugLog("Transaction not confirmed within timeout")
			if sendAttempt < maxSendRetries {
				SimpleLogger.Printf("Warm-up TX confirmation attempt %d timed out, retrying send...", sendAttempt)
				time.Sleep(2 * time.Second)
				continue
			}
			return fmt.Errorf("all warm-up TX confirmation attempts timed out")
		}

		// If we got here, transaction was successful
		WarmupSignature = sig
		SimpleLogger.Printf("Warm-up Tx confirmed after %d attempts: %s", sendAttempt, sig.String())
		return nil
	}

	return fmt.Errorf("failed to send and confirm warm-up TX after %d attempts", sendAttempt)
}

func sendWarmupTransaction(
	ctx context.Context,
	rpcClient *rpc.Client,
	raydiumClient *raydium.Raydium,
	user solana.PublicKey,
	poolKeys *layouts.ApiPoolInfoV4,
	inputToken *utils.Token,
	outputToken *utils.Token,
	slippage *utils.Percent,
) (solana.Signature, error) {
	DebugLog("Building warm-up transaction...")

	amount := utils.NewTokenAmount(inputToken, GlobalConfig.Amount)
	DebugLog("Querying amounts out for warm-up...")

	amountsOut, err := raydiumClient.Liquidity.GetAmountsOut(poolKeys, amount, slippage)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("warmUpTx amountsOut: %w", err)
	}
	DebugLog("Amount in for warm-up: %d, Min amount out: %d", amountsOut.AmountIn.Amount, amountsOut.MinAmountOut.Amount)

	DebugLog("Creating swap transaction for warm-up...")
	swapTx, err := raydiumClient.Trade.MakeSwapTransaction(
		poolKeys,
		amountsOut.AmountIn,
		amountsOut.MinAmountOut,
		trade.FeeConfig{
			MicroLamports: uint64(GlobalConfig.PrioFee * 1e6),
		},
	)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("warmUpTx MakeSwapTx: %w", err)
	}

	hasBudgetInstr := hasComputeBudgetInstruction(
		swapTx.Message.Instructions,
		swapTx.Message.AccountKeys,
	)
	raydiumInstrs := compiledToInstructions(
		swapTx.Message.Instructions,
		swapTx.Message.AccountKeys,
	)

	var prioInstructions []solana.Instruction
	if GlobalConfig.PrioFee > 0 && !hasBudgetInstr {
		prioInstructions = append(prioInstructions,
			computebudget.NewSetComputeUnitPriceInstruction(uint64(GlobalConfig.PrioFee*1e6)).Build(),
			computebudget.NewSetComputeUnitLimitInstruction(uint32(GlobalConfig.ComputeUnitLimit)).Build(),
		)
	}

	memoData := createCleanMemo("thorBench:Warmup[%s]", TestID)
	memoInst := createRawMemoInstruction(memoData, user)

	allInstrs := append(prioInstructions, memoInst)
	allInstrs = append(allInstrs, raydiumInstrs...)

	DebugLog("Getting latest blockhash for warm-up...")
	latest, err := rpcClient.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("warmUpTx GetLatestBlockhash: %w", err)
	}

	DebugLog("Creating transaction for warm-up...")
	finalTx, err := solana.NewTransaction(
		allInstrs,
		latest.Value.Blockhash,
		solana.TransactionPayer(user),
	)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("warmUpTx NewTransaction: %w", err)
	}

	DebugLog("Signing transaction for warm-up...")
	_, err = finalTx.Sign(func(pk solana.PublicKey) *solana.PrivateKey {
		if pk.Equals(user) {
			return TestAccount
		}
		return nil
	})
	if err != nil {
		return solana.Signature{}, fmt.Errorf("signing tx: %w", err)
	}

	// Here we use warmUpRpcRetries (10) instead of GlobalConfig.NodeRetries
	retries := uint(warmUpRpcRetries)
	DebugLog("Sending warm-up transaction with %d retries...", retries)
	sig, err := rpcClient.SendTransactionWithOpts(
		ctx,
		finalTx,
		rpc.TransactionOpts{
			SkipPreflight: false,
			MaxRetries:    &retries,
		},
	)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("SendTransaction: %w", err)
	}

	SimpleLogger.Printf("Warm-up Tx SENT: %s", sig.String())
	return sig, nil
}

func waitForTxConfirmation(ctx context.Context, rpcClient *rpc.Client, sig solana.Signature) (bool, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
			DebugLog("Checking transaction status: %s", sig.String())
			status, err := rpcClient.GetSignatureStatuses(ctx, true, sig)
			if err != nil {
				DebugLog("Error checking transaction status: %v", err)
				continue
			}

			if status == nil {
				DebugLog("Got nil status response")
				continue
			}

			if len(status.Value) == 0 || status.Value[0] == nil {
				DebugLog("Transaction not found in ledger yet")
				continue
			}

			if status.Value[0].Err != nil {
				return false, fmt.Errorf("transaction failed: %v", status.Value[0].Err)
			}

			// Transaction is finalized if Confirmations is nil
			if status.Value[0].Confirmations == nil {
				DebugLog("Transaction is finalized")
				return true, nil
			}

			// Or if we have enough confirmations
			if status.Value[0].Confirmations != nil && *status.Value[0].Confirmations >= 32 {
				DebugLog("Transaction has %d confirmations (>=32), considering confirmed", *status.Value[0].Confirmations)
				return true, nil
			}

			DebugLog("Waiting for confirmations: %d/32",
				func() uint64 {
					if status.Value[0].Confirmations != nil {
						return *status.Value[0].Confirmations
					}
					return 0
				}())
		}
	}
}

// checkStopCondition sees if all TX have landed
func checkStopCondition() {
	Mu.Lock()
	defer Mu.Unlock()

	failed := SlippageFailures + OtherFailures
	totalLanded := ProcessedTransactions + failed
	allToSend := GlobalConfig.TxCount

	// First check if we've sent all we plan to send
	if SentTransactions >= allToSend {
		// Then check if all sent transactions have landed (processed or failed)
		if totalLanded >= SentTransactions && WsListener != nil && WsListener.Listening {
			SimpleLogger.Printf("All transactions have landed (%d/%d). Stopping listener.",
				totalLanded, SentTransactions)

			// Log final stats before stopping
			successRate := float64(ProcessedTransactions) / float64(SentTransactions) * 100
			SimpleLogger.Printf("Final success rate: %.2f%% (%d/%d)",
				successRate, ProcessedTransactions, SentTransactions)

			SimpleLogger.Printf("Stopping listener and preparing final report...")

			// Use a goroutine to stop the listener to avoid deadlock
			go func() {
				time.Sleep(500 * time.Millisecond) // Brief pause to allow logging
				WsListener.Stop()
			}()
		}
	}

	// Only log progress in non-debug mode to keep output clean
	if !GlobalConfig.Debug && totalLanded > 0 && (totalLanded%5 == 0 || totalLanded == SentTransactions) {
		successRate := float64(0)
		if SentTransactions > 0 {
			successRate = float64(ProcessedTransactions) / float64(SentTransactions) * 100
		}

		notLanded := uint64(0)
		if SentTransactions > totalLanded {
			notLanded = SentTransactions - totalLanded
		}

		SimpleLogger.Printf("Progress: %d/%d transactions processed (%.1f%% success, %d not landed)",
			totalLanded, SentTransactions, successRate, notLanded)
	}
}
