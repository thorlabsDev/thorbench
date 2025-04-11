package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Slippage         float64 `json:"slippage"`
	Amount           float64 `json:"amount"`
	ComputeUnitLimit uint64  `json:"compute_unit_limit"`
	SkipWarmup       bool    `json:"skip_warmup"`
	Debug            bool    `json:"debug"`
}

func (c *Config) GetWsUrl() string {
	if c.WsUrl != "" {
		return strings.TrimSuffix(c.WsUrl, "/")
	}
	// Derive from RPC URL if not provided.
	url := strings.ReplaceAll(strings.ReplaceAll(c.RpcUrl, "http://", "ws://"), "https://", "wss://")
	return strings.TrimSuffix(url, "/")
}

func (c *Config) GetSendUrl() string {
	if c.SendRpcUrl != "" {
		return strings.TrimSuffix(c.SendRpcUrl, "/")
	}
	return strings.TrimSuffix(c.RpcUrl, "/")
}

func getExecutablePath() string {
	ex, err := os.Executable()
	if err != nil {
		SimpleLogger.Printf("Warning: Could not determine executable path: %v", err)
		return "."
	}
	dir := filepath.Dir(ex)
	return dir
}

func findConfigFile() (string, error) {
	execDir := getExecutablePath()

	// Always try executable directory first
	configPath := filepath.Join(execDir, "config.json")

	if _, err := os.Stat(configPath); err == nil {
		if data, err := os.ReadFile(configPath); err == nil && len(data) > 0 {
			return configPath, nil
		} else {
			SimpleLogger.Printf("Warning: Found config in executable directory but couldn't read it: %v", err)
		}
	}

	// If not found in executable directory, try current directory
	cwd, err := os.Getwd()
	if err != nil {
		SimpleLogger.Printf("Warning: Could not determine current working directory: %v", err)
		cwd = "."
	}

	configPath = filepath.Join(cwd, "config.json")

	if _, err := os.Stat(configPath); err == nil {
		if data, err := os.ReadFile(configPath); err == nil && len(data) > 0 {
			SimpleLogger.Printf("Successfully read %d bytes from config in current directory", len(data))
			return configPath, nil
		} else {
			SimpleLogger.Printf("Warning: Found config in current directory but couldn't read it: %v", err)
		}
	}

	return "", fmt.Errorf("config file not found")
}

func ReadConfig() *Config {
	configPath, err := findConfigFile()
	if err != nil {
		SimpleLogger.Printf("No existing config file found, creating template...")
		configTemplate := DEFAULT_CONFIG
		if err := WriteConfig(&configTemplate); err != nil {
			SimpleLogger.Fatalf("Error creating config file: %v", err)
		}
		SimpleLogger.Printf("\nExample config.json created at: %s\nPlease edit all required fields, then restart.\n", configPath)
		os.Exit(1)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		SimpleLogger.Fatalf("Error reading config file at %s: %v", configPath, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		SimpleLogger.Fatalf("Error parsing config JSON: %v\nContent: %s", err, string(data))
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

	configPath := filepath.Join(getExecutablePath(), "config.json")
	SimpleLogger.Printf("Writing config to: %s", configPath)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("error creating config directory: %w", err)
	}

	return os.WriteFile(configPath, data, 0644)
}

func VerifyPrivateKey(base58key string) {
	account, err := solana.PrivateKeyFromBase58(base58key)
	if err != nil {
		SimpleLogger.Fatalf("error parsing private key: %v", err)
	}
	TestAccount = &account
}

func AssertSufficientBalance() {
	rpcClient := InitializeRPCClient(GlobalConfig.RpcUrl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var balance *rpc.GetBalanceResult
	var err error

	for retries := 0; retries < 3; retries++ {
		balance, err = rpcClient.GetBalance(ctx, TestAccount.PublicKey(), rpc.CommitmentFinalized)
		if err == nil && balance != nil && balance.Value > 0 {
			break
		}
		if ctx.Err() != nil {
			SimpleLogger.Fatalf("timeout getting test wallet balance after %d retries: %v", retries, err)
		}
		time.Sleep(time.Second * time.Duration(retries+1))
	}

	if err != nil || balance == nil {
		SimpleLogger.Fatalf("error getting test wallet balance after retries: %v", err)
	}

	if balance.Value <= 0 {
		SimpleLogger.Fatal("received invalid zero or negative balance from RPC")
	}

	costPerTx := uint64(GlobalConfig.PrioFee*float64(GlobalConfig.ComputeUnitLimit)) + 5000
	totalCost := GlobalConfig.TxCount * costPerTx
	if uint64(balance.Value) < totalCost/2 {
		SimpleLogger.Fatal(
			"Insufficient balance in test wallet.",
			"balance", fmt.Sprintf("%.6f SOL", float64(balance.Value)/1_000_000_000),
			"required", fmt.Sprintf("%.6f SOL", float64(totalCost)/1_000_000_000),
		)
	}
}
