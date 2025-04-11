package internal

import (
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gagliardetto/solana-go"
	"golang.org/x/time/rate"
)

// Example token mints for RAY <-> SOL
var (
	// Raydium's RAY token
	RAYMint = solana.MustPublicKeyFromBase58("4k3Dyjzvzp8eMZWUXbBCjEvwSkkk59S5iCNLY3QrkX6R")
	// Native SOL mint
	SOLMint = solana.MustPublicKeyFromBase58("So11111111111111111111111111111111111111112")
)

var Version = "1.1.0"

var (
	// Default config can be used if config.json doesn't exist
	DEFAULT_CONFIG = Config{
		RpcUrl:           "http://rpc-de.thornode.io/",
		RateLimit:        200,
		NodeRetries:      0,
		TxCount:          20,
		PrioFee:          1.0,
		ComputeUnitLimit: 30000,
		InputMint:        "So11111111111111111111111111111111111111112",
		OutputMint:       "4k3Dyjzvzp8eMZWUXbBCjEvwSkkk59S5iCNLY3QrkX6R",
		Slippage:         100.0,
		Amount:           0.001,
		SkipWarmup:       false,
		Debug:            false,
	}
)

// BlockHeightRecord stores a block height with its timestamp
type BlockHeightRecord struct {
	Height    uint64
	Timestamp time.Time
	TxSig     solana.Signature // Associated transaction signature
}

// Global variables
var (
	GlobalConfig *Config
	TestAccount  *solana.PrivateKey

	// For concurrency / stats:
	SentTransactions      uint64
	ProcessedTransactions uint64
	SlippageFailures      uint64
	OtherFailures         uint64

	TxTimes            = make(map[solana.Signature]time.Time)
	TxBlocks           = make(map[uint64]uint64)
	BlockTimes         = make(map[uint64]time.Time)
	BlockHeightHistory = make([]BlockHeightRecord, 0, 1000) // Store block heights with timestamps
	TxLandBlockHeights = make(map[solana.Signature]uint64)

	TestID      string
	LogFileName = "benchmark.log"

	WsListener   *WebsocketListener
	SimpleLogger *log.Logger

	// synchronization
	Wg sync.WaitGroup
	Mu sync.RWMutex

	// Rate-limiter
	Limiter = rate.NewLimiter(rate.Limit(200), 200)

	// The time we will force-stop listening
	StopTime time.Time
)

// DebugLog prints a message only when debug mode is enabled
func DebugLog(format string, args ...interface{}) {
	if GlobalConfig != nil && GlobalConfig.Debug {
		SimpleLogger.Printf(format, args...)
	}
}
