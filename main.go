package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	lapi "github.com/filecoin-project/lotus/api"
	"github.com/joho/godotenv"
	lclient "github.com/filecoin-project/lotus/api/client"
	"github.com/filecoin-project/lotus/api/v0api"
)

type config struct {
	RPCURL           string
	ContractAddress  common.Address
	ProviderID       uint64
	LotusAPIURL      string
	LotusAPIToken    string
	MinerAPIURL      string
	MinerAPIToken    string
	DDOContractF4    string
	PollInterval     time.Duration
	StartBlock       string // "latest" or a block number
	DownloadDir      string
	StartEpochBuffer int64
	EndEpochBuffer   int64
}

func loadConfig() (*config, error) {
	cfg := &config{
		PollInterval:     30 * time.Second,
		StartBlock:       "latest",
		DownloadDir:      "./downloads",
		StartEpochBuffer: 5760,
		EndEpochBuffer:   1152000,
	}

	// Required env vars
	cfg.RPCURL = os.Getenv("RPC_URL")
	if cfg.RPCURL == "" {
		return nil, fmt.Errorf("RPC_URL is required")
	}

	contractStr := os.Getenv("CONTRACT_ADDRESS")
	if contractStr == "" {
		return nil, fmt.Errorf("CONTRACT_ADDRESS is required")
	}
	if !common.IsHexAddress(contractStr) {
		return nil, fmt.Errorf("CONTRACT_ADDRESS %q is not a valid hex address", contractStr)
	}
	cfg.ContractAddress = common.HexToAddress(contractStr)

	providerStr := os.Getenv("PROVIDER_ID")
	if providerStr == "" {
		return nil, fmt.Errorf("PROVIDER_ID is required")
	}
	pid, err := strconv.ParseUint(providerStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("PROVIDER_ID must be a valid uint64: %w", err)
	}
	cfg.ProviderID = pid

	cfg.LotusAPIURL = os.Getenv("LOTUS_API_URL")
	if cfg.LotusAPIURL == "" {
		return nil, fmt.Errorf("LOTUS_API_URL is required")
	}
	cfg.LotusAPIToken = os.Getenv("LOTUS_API_TOKEN")
	if cfg.LotusAPIToken == "" {
		return nil, fmt.Errorf("LOTUS_API_TOKEN is required")
	}

	cfg.MinerAPIURL = os.Getenv("MINER_API_URL")
	if cfg.MinerAPIURL == "" {
		return nil, fmt.Errorf("MINER_API_URL is required")
	}
	cfg.MinerAPIToken = os.Getenv("MINER_API_TOKEN")
	if cfg.MinerAPIToken == "" {
		return nil, fmt.Errorf("MINER_API_TOKEN is required")
	}

	cfg.DDOContractF4 = os.Getenv("DDO_CONTRACT_F4")
	if cfg.DDOContractF4 == "" {
		return nil, fmt.Errorf("DDO_CONTRACT_F4 is required")
	}

	// Optional env vars
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("POLL_INTERVAL must be a valid duration: %w", err)
		}
		cfg.PollInterval = d
	}
	if v := os.Getenv("START_BLOCK"); v != "" {
		cfg.StartBlock = v
	}
	if v := os.Getenv("DOWNLOAD_DIR"); v != "" {
		cfg.DownloadDir = v
	}
	if v := os.Getenv("START_EPOCH_BUFFER"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("START_EPOCH_BUFFER must be a valid int64: %w", err)
		}
		cfg.StartEpochBuffer = n
	}
	if v := os.Getenv("END_EPOCH_BUFFER"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("END_EPOCH_BUFFER must be a valid int64: %w", err)
		}
		cfg.EndEpochBuffer = n
	}

	return cfg, nil
}

func authHeader(token string) http.Header {
	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+token)
	return headers
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Load .env file if present (does not override existing env vars)
	_ = godotenv.Load()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	// Connect to Ethereum RPC (Lotus gateway or public endpoint)
	log.Printf("Connecting to Ethereum RPC: %s", cfg.RPCURL)
	ethClient, err := ethclient.DialContext(ctx, cfg.RPCURL)
	if err != nil {
		log.Fatalf("Failed to connect to Ethereum RPC: %v", err)
	}
	defer ethClient.Close()

	// Connect to Lotus full node
	log.Printf("Connecting to Lotus full node: %s", cfg.LotusAPIURL)
	fullNode, fnCloser, err := lclient.NewFullNodeRPCV1(ctx, cfg.LotusAPIURL, authHeader(cfg.LotusAPIToken))
	if err != nil {
		log.Fatalf("Failed to connect to Lotus full node: %v", err)
	}
	defer fnCloser()

	// Connect to Lotus miner (with ReaderParamEncoder for streaming piece data)
	log.Printf("Connecting to Lotus miner: %s", cfg.MinerAPIURL)
	minerAPI, minerCloser, err := lclient.NewStorageMinerRPCV0(ctx, cfg.MinerAPIURL, authHeader(cfg.MinerAPIToken))
	if err != nil {
		log.Fatalf("Failed to connect to Lotus miner: %v", err)
	}
	defer minerCloser()

	// Determine starting block
	var lastBlock uint64
	if cfg.StartBlock == "latest" {
		header, err := ethClient.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Fatalf("Failed to get latest block: %v", err)
		}
		lastBlock = header.Number.Uint64()
		log.Printf("Starting from latest block: %d", lastBlock)
	} else {
		n, err := strconv.ParseUint(cfg.StartBlock, 10, 64)
		if err != nil {
			log.Fatalf("Invalid START_BLOCK %q: %v", cfg.StartBlock, err)
		}
		lastBlock = n
		log.Printf("Starting from block: %d", lastBlock)
	}

	log.Printf("Polling for AllocationCreated events (provider=%d, interval=%s)",
		cfg.ProviderID, cfg.PollInterval)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	// Run an initial poll immediately
	lastBlock = pollEvents(ctx, cfg, ethClient, fullNode, minerAPI, lastBlock)

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down...")
			return
		case <-ticker.C:
			lastBlock = pollEvents(ctx, cfg, ethClient, fullNode, minerAPI, lastBlock)
		}
	}
}

// pollEvents fetches new blocks since lastBlock, filters AllocationCreated events
// for this provider, processes each matching allocation, and returns the new lastBlock.
func pollEvents(ctx context.Context, cfg *config, ethClient *ethclient.Client, fullNode lapi.FullNode, minerAPI v0api.StorageMiner, lastBlock uint64) uint64 {
	header, err := ethClient.HeaderByNumber(ctx, nil)
	if err != nil {
		log.Printf("ERROR: get latest block: %v", err)
		return lastBlock
	}
	currentBlock := header.Number.Uint64()

	if currentBlock <= lastBlock {
		return lastBlock
	}

	fromBlock := new(big.Int).SetUint64(lastBlock + 1)
	toBlock := new(big.Int).SetUint64(currentBlock)

	log.Printf("Scanning blocks %d to %d", lastBlock+1, currentBlock)

	events, err := fetchAllocationEvents(ctx, ethClient, cfg.ContractAddress, fromBlock, toBlock)
	if err != nil {
		log.Printf("ERROR: fetch events: %v", err)
		return lastBlock
	}

	for _, evt := range events {
		// Filter by our provider ID
		if evt.Provider != cfg.ProviderID {
			continue
		}

		log.Printf("Found allocation %d for provider %d from client %s",
			evt.AllocationID, evt.Provider, evt.Client.Hex())

		if err := processAllocation(ctx, cfg, fullNode, minerAPI, evt); err != nil {
			log.Printf("ERROR: process allocation %d: %v", evt.AllocationID, err)
			continue
		}

		log.Printf("Successfully onboarded allocation %d", evt.AllocationID)
	}

	return currentBlock
}
