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

func SendTransactions() {
	rpcClient := rpc.New(GlobalConfig.GetSendUrl())
	raydiumClient := raydium.New(rpcClient, GlobalConfig.PrivateKey)

	user := TestAccount.PublicKey()

	// 1) Decide which mints to trade
	inMint := GlobalConfig.InputMint
	outMint := GlobalConfig.OutputMint
	if inMint == "" {
		inMint = SOLMint.String()
	}
	if outMint == "" {
		outMint = RAYMint.String()
	}

	inMintPK := solana.MustPublicKeyFromBase58(inMint)
	outMintPK := solana.MustPublicKeyFromBase58(outMint)

	// 2) Fetch decimals
	inDecimals, err := FetchDecimalsForMint(rpcClient, inMintPK)
	if err != nil {
		SimpleLogger.Fatalf("error fetching decimals for input mint %s: %v", inMint, err)
	}
	outDecimals, err := FetchDecimalsForMint(rpcClient, outMintPK)
	if err != nil {
		SimpleLogger.Fatalf("error fetching decimals for output mint %s: %v", outMint, err)
	}

	inputToken := utils.NewToken("INPUT", inMint, uint64(inDecimals))
	outputToken := utils.NewToken("OUTPUT", outMint, uint64(outDecimals))

	// 3) Get Raydium pool keys
	poolKeys, err := raydiumClient.Pool.GetPoolKeys(inputToken.Mint, outputToken.Mint)
	if err != nil {
		SimpleLogger.Fatalf("error getting pool keys: %v", err)
	}

	// 4) Convert config slippage
	slipValue := int64(GlobalConfig.Slippage * 100)
	slippage := utils.NewPercent(uint64(slipValue), 10000)

	// 5) Do warm-up TX if not skipped
	if !GlobalConfig.SkipWarmup {
		warmUpSuccess := false
		for attempt := 1; attempt <= warmUpMaxRetries; attempt++ {
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
						if err == nil && status != nil && len(status.Value) > 0 && status.Value[0] != nil {
							txStatus := status.Value[0]

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
									SimpleLogger.Printf("Warm-up TX has %d confirmations, waiting for >%d...", confirmations, minConfirmations)
								}
							}
						}
						time.Sleep(2 * time.Second)
					}
				}
			}
			SimpleLogger.Printf("Warm-up TX attempt %d failed: %v", attempt, err)
			time.Sleep(2 * time.Second)
		}
		if !warmUpSuccess {
			SimpleLogger.Fatal("Warm-up TX failed after all retries. Aborting.")
		}
	}

START_BENCHMARK:
	// 6) Run concurrent transactions
	amount := utils.NewTokenAmount(inputToken, GlobalConfig.Amount)

	startTime := time.Now().Truncate(5 * time.Second).Add(10 * time.Second)
	for i := uint64(0); i < GlobalConfig.TxCount; i++ {
		go func(id uint64) {
			sleepTime := time.Until(startTime)
			if id == 1 {
				SimpleLogger.Print("Threads sleeping until benchmark start", "delay", sleepTime.Truncate(time.Millisecond))
			}
			time.Sleep(sleepTime)

			// Rate limiter
			if err := Limiter.Wait(context.TODO()); err != nil {
				Mu.Lock()
				OtherFailures++
				Mu.Unlock()
				checkStopCondition()
				return
			}

			// Query amounts
			amountsOut, err := raydiumClient.Liquidity.GetAmountsOut(poolKeys, amount, slippage)
			if err != nil {
				Mu.Lock()
				OtherFailures++
				Mu.Unlock()
				checkStopCondition()
				return
			}

			// Build swap
			swapTx, err := raydiumClient.Trade.MakeSwapTransaction(
				poolKeys,
				amountsOut.AmountIn,
				amountsOut.MinAmountOut,
				trade.FeeConfig{
					MicroLamports: uint64(GlobalConfig.PrioFee * 1e6),
				},
			)
			if err != nil {
				Mu.Lock()
				OtherFailures++
				Mu.Unlock()
				checkStopCondition()
				return
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

			memoData := createCleanMemo("thorBench:Test%d[%s]", id, TestID)
			memoInst := createRawMemoInstruction(memoData, user)

			allInstrs := append(prioInstructions, memoInst)
			allInstrs = append(allInstrs, raydiumInstrs...)

			latest, err := rpcClient.GetLatestBlockhash(context.TODO(), rpc.CommitmentFinalized)
			if err != nil {
				Mu.Lock()
				OtherFailures++
				Mu.Unlock()
				checkStopCondition()
				return
			}

			finalTx, err := solana.NewTransaction(
				allInstrs,
				latest.Value.Blockhash,
				solana.TransactionPayer(user),
			)
			if err != nil {
				Mu.Lock()
				OtherFailures++
				Mu.Unlock()
				checkStopCondition()
				return
			}

			_, err = finalTx.Sign(func(pk solana.PublicKey) *solana.PrivateKey {
				if pk.Equals(user) {
					return TestAccount
				}
				return nil
			})
			if err != nil {
				Mu.Lock()
				OtherFailures++
				Mu.Unlock()
				checkStopCondition()
				return
			}

			sig, err := rpcClient.SendTransactionWithOpts(
				context.TODO(),
				finalTx,
				rpc.TransactionOpts{
					SkipPreflight: true,
					MaxRetries:    &GlobalConfig.NodeRetries,
				},
			)
			if err != nil {
				Mu.Lock()
				OtherFailures++
				Mu.Unlock()
				checkStopCondition()
				return
			}

			Mu.Lock()
			SentTransactions++
			TxTimes[sig] = time.Now()
			Mu.Unlock()

			SimpleLogger.Print("Tx SENT",
				"threadID", id,
				"sig", sig.String(),
			)

			checkStopCondition()
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

	// Keep trying to send until we get confirmation or hit max retries
	for sendAttempt = 1; sendAttempt <= maxSendRetries; sendAttempt++ {
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

		// Wait for the transaction to be confirmed
		confirmCtx, cancel := context.WithTimeout(context.Background(), confirmTimeout)
		confirmed, err := waitForTxConfirmation(confirmCtx, rpcClient, sig)
		cancel()

		if err != nil || !confirmed {
			if sendAttempt < maxSendRetries {
				SimpleLogger.Printf("Warm-up TX confirmation attempt %d failed or timed out, retrying send...", sendAttempt)
				time.Sleep(2 * time.Second)
				continue
			}
			return fmt.Errorf("all warm-up TX confirmation attempts failed")
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
	amount := utils.NewTokenAmount(inputToken, GlobalConfig.Amount)
	amountsOut, err := raydiumClient.Liquidity.GetAmountsOut(poolKeys, amount, slippage)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("warmUpTx amountsOut: %w", err)
	}

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

	latest, err := rpcClient.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("warmUpTx GetLatestBlockhash: %w", err)
	}

	finalTx, err := solana.NewTransaction(
		allInstrs,
		latest.Value.Blockhash,
		solana.TransactionPayer(user),
	)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("warmUpTx NewTransaction: %w", err)
	}

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
			status, err := rpcClient.GetSignatureStatuses(ctx, true, sig)
			if err != nil {
				SimpleLogger.Printf("Error checking transaction status: %v", err)
				continue
			}

			if status != nil && len(status.Value) > 0 && status.Value[0] != nil {
				if status.Value[0].Err != nil {
					return false, fmt.Errorf("transaction failed: %v", status.Value[0].Err)
				}

				// Transaction is finalized if Confirmations is nil
				if status.Value[0].Confirmations == nil {
					return true, nil
				}

				// Or if we have enough confirmations
				if status.Value[0].Confirmations != nil && *status.Value[0].Confirmations >= 32 {
					return true, nil
				}

				SimpleLogger.Printf("Waiting for confirmations: %d/32",
					func() uint64 {
						if status.Value[0].Confirmations != nil {
							return *status.Value[0].Confirmations
						}
						return 0
					}())
			}
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

	if SentTransactions >= allToSend {
		if totalLanded >= SentTransactions && WsListener != nil && WsListener.Listening {
			WsListener.Stop()
		}
	}
}
