package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type Config struct {
	PrivateKey       string  `json:"private_key"`
	RpcUrl           string  `json:"rpc_url"`
	WsUrl            string  `json:"ws_url"`
	SendRpcUrl       string  `json:"send_rpc_url"`
	RateLimit        uint64  `json:"rate_limit"`
	TxCount          uint64  `json:"tx_count"`
	PrioFee          float64 `json:"prio_fee"`
	NodeRetries      uint    `json:"node_retries"`
	InputMint        string  `json:"input_mint"`
	OutputMint       string  `json:"output_mint"`
	Slippage         float64 `json:"slippage"` // e.g. 1.0 => 1%
	Amount           float64 `json:"amount"`   // tokens to swap
	ComputeUnitLimit uint64  `json:"compute_unit_limit"`
	SkipWarmup       bool    `json:"skip_warmup"` // NEW: option to skip warmup TX
}

func (c *Config) GetWsUrl() string {
	if c.WsUrl != "" {
		return c.WsUrl
	}
	return strings.ReplaceAll(strings.ReplaceAll(c.RpcUrl, "http://", "ws://"), "https://", "wss://")
}

func (c *Config) GetSendUrl() string {
	if c.SendRpcUrl != "" {
		return c.SendRpcUrl
	}
	return c.RpcUrl
}

func ReadConfig() *Config {
	configTemplate := DEFAULT_CONFIG
	data, err := os.ReadFile("config.json")
	if err != nil {
		if os.IsNotExist(err) {
			if err := WriteConfig(&configTemplate); err != nil {
				SimpleLogger.Fatalf("error creating config file: %v", err)
			}
			SimpleLogger.Printf("\nExample config.json created. Please edit all required fields, then restart.\n")
			os.Exit(1)
		}
		SimpleLogger.Fatalf("error opening config file: %v", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		SimpleLogger.Fatalf("error parsing config file: %v", err)
	}

	// Validate required fields
	var missing []string
	if cfg.PrivateKey == "" || cfg.PrivateKey == "<YOUR-BASE58-PRIVATE-KEY>" {
		missing = append(missing, "private_key")
	}
	if cfg.RpcUrl == "" {
		missing = append(missing, "rpc_url")
	}
	if cfg.RateLimit == 0 {
		missing = append(missing, "rate_limit")
	}
	if cfg.TxCount == 0 {
		missing = append(missing, "tx_count")
	}
	if cfg.Slippage == 0 {
		missing = append(missing, "slippage")
	}
	if cfg.Amount == 0 {
		missing = append(missing, "amount")
	}
	if len(missing) > 0 {
		SimpleLogger.Fatal("Missing required config fields: " + strings.Join(missing, ", "))
	}
	if cfg.ComputeUnitLimit == 0 {
		cfg.ComputeUnitLimit = 30000
	}

	return &cfg
}

func WriteConfig(config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling config: %w", err)
	}
	return os.WriteFile("config.json", data, 0644)
}

func VerifyPrivateKey(base58key string) {
	account, err := solana.PrivateKeyFromBase58(base58key)
	if err != nil {
		SimpleLogger.Fatalf("error parsing private key: %v", err)
	}
	TestAccount = &account
}

func AssertSufficientBalance() {
	rpcClient := rpc.New(GlobalConfig.RpcUrl)
	balance, err := rpcClient.GetBalance(context.TODO(), TestAccount.PublicKey(), rpc.CommitmentFinalized)
	if err != nil || balance == nil {
		SimpleLogger.Fatalf("error getting test wallet balance: %v", err)
	}

	costPerTx := uint64(GlobalConfig.PrioFee*float64(GlobalConfig.ComputeUnitLimit)) + 5000
	totalCost := GlobalConfig.TxCount * costPerTx
	if balance.Value < totalCost/2 {
		SimpleLogger.Fatal(
			"Insufficient balance in test wallet.",
			"balance", fmt.Sprintf("%.6f SOL", float64(balance.Value)/1_000_000_000),
			"required", fmt.Sprintf("%.6f SOL", float64(totalCost)/1_000_000_000),
		)
	}
}
