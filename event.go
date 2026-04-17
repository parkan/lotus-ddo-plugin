package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	cid "github.com/ipfs/go-cid"
)

// AllocationCreatedEvent represents a parsed AllocationCreated event from the DDO contract.
type AllocationCreatedEvent struct {
	Client       common.Address
	AllocationID uint64
	Provider     uint64
	Data         []byte // raw CID bytes
	Size         uint64
	TermMin      int64
	TermMax      int64
	Expiration   int64
	DownloadURL  string
}

// ABI JSON for the AllocationCreated event (from DDOTypes.sol).
const allocationCreatedABIJSON = `[{
	"type": "event",
	"name": "AllocationCreated",
	"anonymous": false,
	"inputs": [
		{"name": "client", "type": "address", "indexed": true, "internalType": "address"},
		{"name": "allocationId", "type": "uint64", "indexed": true, "internalType": "uint64"},
		{"name": "provider", "type": "uint64", "indexed": true, "internalType": "uint64"},
		{"name": "data", "type": "bytes", "indexed": false, "internalType": "bytes"},
		{"name": "size", "type": "uint64", "indexed": false, "internalType": "uint64"},
		{"name": "termMin", "type": "int64", "indexed": false, "internalType": "int64"},
		{"name": "termMax", "type": "int64", "indexed": false, "internalType": "int64"},
		{"name": "expiration", "type": "int64", "indexed": false, "internalType": "int64"},
		{"name": "downloadURL", "type": "string", "indexed": false, "internalType": "string"}
	]
}]`

var (
	allocationCreatedABI   abi.ABI
	allocationCreatedTopic common.Hash
)

func init() {
	parsed, err := abi.JSON(strings.NewReader(allocationCreatedABIJSON))
	if err != nil {
		panic("failed to parse AllocationCreated ABI: " + err.Error())
	}
	allocationCreatedABI = parsed
	allocationCreatedTopic = crypto.Keccak256Hash(
		[]byte("AllocationCreated(address,uint64,uint64,bytes,uint64,int64,int64,int64,string)"),
	)
}

// bytesToCID parses raw CID bytes from the event data field.
func bytesToCID(data []byte) (cid.Cid, error) {
	_, c, err := cid.CidFromBytes(data)
	if err != nil {
		return cid.Undef, fmt.Errorf("failed to parse CID from bytes: %w", err)
	}
	return c, nil
}

// parseAllocationCreatedLog extracts an AllocationCreatedEvent from a raw Ethereum log.
// Indexed fields (client, allocationId, provider) come from topics[1-3].
// Non-indexed fields (data, size, termMin, termMax, expiration, downloadURL) are ABI-decoded from log.Data.
func parseAllocationCreatedLog(vLog types.Log) (*AllocationCreatedEvent, error) {
	if len(vLog.Topics) < 4 {
		return nil, fmt.Errorf("expected 4 topics, got %d", len(vLog.Topics))
	}

	evt := &AllocationCreatedEvent{}

	// Indexed fields from topics (each topic is 32 bytes, values are right-aligned)
	evt.Client = common.BytesToAddress(vLog.Topics[1].Bytes())
	evt.AllocationID = binary.BigEndian.Uint64(vLog.Topics[2].Bytes()[24:])
	evt.Provider = binary.BigEndian.Uint64(vLog.Topics[3].Bytes()[24:])

	// Non-indexed fields via ABI unpack
	nonIndexed := allocationCreatedABI.Events["AllocationCreated"].Inputs.NonIndexed()
	values, err := nonIndexed.UnpackValues(vLog.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack event data: %w", err)
	}
	if len(values) < 6 {
		return nil, fmt.Errorf("expected 6 non-indexed values, got %d", len(values))
	}

	evt.Data = values[0].([]byte)
	evt.Size = values[1].(uint64)
	evt.TermMin = values[2].(int64)
	evt.TermMax = values[3].(int64)
	evt.Expiration = values[4].(int64)
	evt.DownloadURL = values[5].(string)

	return evt, nil
}

// fetchAllocationEvents queries the Ethereum client for AllocationCreated logs within a block range.
// providerID is always pushed to Topics[3] so the RPC node pre-filters by provider.
// clientAllowlist, if non-empty, is pushed to Topics[1] so only allowlisted clients are returned.
func fetchAllocationEvents(ctx context.Context, client *ethclient.Client, contractAddr common.Address, fromBlock, toBlock *big.Int, providerID uint64, clientAllowlist map[common.Address]struct{}) ([]AllocationCreatedEvent, error) {
	// Topic positions: [0]=signature, [1]=client, [2]=allocationID, [3]=provider.
	topics := make([][]common.Hash, 4)
	topics[0] = []common.Hash{allocationCreatedTopic}
	if len(clientAllowlist) > 0 {
		clientTopics := make([]common.Hash, 0, len(clientAllowlist))
		for addr := range clientAllowlist {
			var h common.Hash
			copy(h[12:], addr[:]) // 20-byte addr right-aligned in 32-byte topic
			clientTopics = append(clientTopics, h)
		}
		topics[1] = clientTopics
	}
	// Topics[2] (allocationID) left nil -- match any.
	var providerTopic common.Hash
	binary.BigEndian.PutUint64(providerTopic[24:], providerID)
	topics[3] = []common.Hash{providerTopic}

	query := ethereum.FilterQuery{
		FromBlock: fromBlock,
		ToBlock:   toBlock,
		Addresses: []common.Address{contractAddr},
		Topics:    topics,
	}

	logs, err := client.FilterLogs(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to filter logs: %w", err)
	}

	var events []AllocationCreatedEvent
	for _, l := range logs {
		evt, err := parseAllocationCreatedLog(l)
		if err != nil {
			log.Printf("WARN: skipping log in block %d tx %s: %v", l.BlockNumber, l.TxHash.Hex(), err)
			continue
		}
		events = append(events, *evt)
	}

	return events, nil
}
