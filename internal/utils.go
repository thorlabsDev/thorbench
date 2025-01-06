package internal

import (
	"context"
	"fmt"
	"github.com/fatih/color"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// compiledToInstructions re-constructs a list of solana.Instruction
func compiledToInstructions(
	cInstrs []solana.CompiledInstruction,
	accountKeys []solana.PublicKey,
) []solana.Instruction {
	out := make([]solana.Instruction, 0, len(cInstrs))

	isWritable := make(map[string]bool)
	isSigner := make(map[string]bool)

	// Mark the first account (fee payer) as signer + writable
	for i, key := range accountKeys {
		keyStr := key.String()
		if i == 0 {
			isWritable[keyStr] = true
			isSigner[keyStr] = true
		} else {
			isWritable[keyStr] = true
			isSigner[keyStr] = false
		}
	}

	for _, ci := range cInstrs {
		var metas []*solana.AccountMeta
		for _, accIndex := range ci.Accounts {
			key := accountKeys[accIndex]
			keyStr := key.String()
			am := &solana.AccountMeta{
				PublicKey:  key,
				IsSigner:   isSigner[keyStr],
				IsWritable: isWritable[keyStr],
			}
			metas = append(metas, am)
		}
		programID := accountKeys[ci.ProgramIDIndex]
		inst := solana.NewInstruction(programID, metas, ci.Data)
		out = append(out, inst)
	}
	return out
}

func hasComputeBudgetInstruction(
	instructions []solana.CompiledInstruction,
	accountKeys []solana.PublicKey,
) bool {
	computeBudgetProgram := solana.MustPublicKeyFromBase58("ComputeBudget111111111111111111111111111111")
	for _, instr := range instructions {
		programID := accountKeys[instr.ProgramIDIndex]
		if programID.Equals(computeBudgetProgram) {
			return true
		}
	}
	return false
}

func PrintBanner() {
	blue := color.New(color.FgBlue).SprintFunc()

	fmt.Println(blue("████████╗██╗  ██╗ ██████╗ ██████╗    ██╗      █████╗ ██████╗ ███████╗"))
	fmt.Println(blue("╚══██╔══╝██║  ██║██╔═══██╗██╔══██╗   ██║     ██╔══██╗██╔══██╗██╔════╝"))
	fmt.Println(blue("   ██║   ███████║██║   ██║██████╔╝   ██║     ███████║██████╔╝███████╗"))
	fmt.Println(blue("   ██║   ██╔══██║██║   ██║██╔══██╗   ██║     ██╔══██║██╔══██╗╚════██║"))
	fmt.Println(blue("   ██║   ██║  ██║╚██████╔╝██║  ██║   ███████╗██║  ██║██████╔╝███████║"))
	fmt.Println(blue("   ╚═╝   ╚═╝  ╚═╝╚═════╝ ╚═╝  ╚═╝   ╚══════╝╚═╝  ╚═╝╚═════╝ ╚══════╝"))
	fmt.Println()

	fmt.Println(blue("https://discord.gg/thorlabs"))
	fmt.Println(blue("https://x.com/thor_labs\n\n"))
	fmt.Printf("RPC Swap Benchmark v%s\n", Version)
	fmt.Println("Solana RPC Benchmark Tool using Raydium Swaps")
	fmt.Println()
}

// FetchDecimalsForMint fetches the "decimals" byte from an SPL Token mint account.
func FetchDecimalsForMint(rpcClient *rpc.Client, mintPubkey solana.PublicKey) (uint8, error) {
	acct, err := rpcClient.GetAccountInfo(context.TODO(), mintPubkey)
	if err != nil {
		return 0, fmt.Errorf("error getting mint account for %s: %v", mintPubkey, err)
	}
	if acct == nil || acct.Value == nil {
		return 0, fmt.Errorf("no account data for %s", mintPubkey)
	}

	// Convert to raw bytes:
	dataBytes := acct.Value.Data.GetBinary()
	if dataBytes == nil {
		return 0, fmt.Errorf("account data is nil for %s", mintPubkey)
	}

	if len(dataBytes) < 45 {
		return 0, fmt.Errorf("invalid mint account size for %s", mintPubkey)
	}

	decimals := dataBytes[44]
	return decimals, nil
}

func createCleanMemo(format string, args ...any) []byte {
	// Create the formatted string
	memo := fmt.Sprintf(format, args...)

	// Convert to bytes, being careful to only include printable ASCII
	cleanBytes := make([]byte, 0, len(memo))
	for _, b := range []byte(memo) {
		// Only include printable ASCII characters (32-126)
		if b >= 32 && b <= 126 {
			cleanBytes = append(cleanBytes, b)
		}
	}
	return cleanBytes
}

// createRawMemoInstruction creates a memo instruction without Borsh encoding
func createRawMemoInstruction(data []byte, signer solana.PublicKey) solana.Instruction {
	return solana.NewInstruction(
		solana.MemoProgramID,
		[]*solana.AccountMeta{
			{
				PublicKey:  signer,
				IsSigner:   true,
				IsWritable: false,
			},
		},
		data,
	)
}
