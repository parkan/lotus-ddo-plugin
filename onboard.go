package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/filecoin-project/go-address"
	cborutil "github.com/filecoin-project/go-cbor-util"
	"github.com/filecoin-project/go-state-types/abi"
	minertypes "github.com/filecoin-project/go-state-types/builtin/v13/miner"
	verifregtypes "github.com/filecoin-project/go-state-types/builtin/v13/verifreg"
	lapi "github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/api/v0api"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/types/ethtypes"
	"github.com/filecoin-project/lotus/storage/pipeline/piece"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// processAllocation handles a single AllocationCreated event: downloads the piece data,
// verifies the CID, resolves the client actor, and onboards the piece into the Lotus miner.
func processAllocation(ctx context.Context, cfg *config, fullNode lapi.FullNode, minerAPI v0api.StorageMiner, evt AllocationCreatedEvent) error {
	// 1. Parse CID from event data bytes
	pieceCID, err := bytesToCID(evt.Data)
	if err != nil {
		return fmt.Errorf("parse piece CID: %w", err)
	}
	log.Printf("Processing allocation %d: pieceCID=%s size=%d url=%s",
		evt.AllocationID, pieceCID, evt.Size, evt.DownloadURL)

	// 2. Download file or use local fallback
	downloadPath := filepath.Join(cfg.DownloadDir, fmt.Sprintf("allocation_%d.car", evt.AllocationID))
	if evt.DownloadURL != "" {
		if err := downloadFile(ctx, evt.DownloadURL, downloadPath); err != nil {
			return fmt.Errorf("download file: %w", err)
		}
		defer os.Remove(downloadPath)
	} else {
		// Fallback: look for a pre-existing file in the download dir
		localPath := filepath.Join(cfg.DownloadDir, "file.car")
		if _, err := os.Stat(localPath); err != nil {
			return fmt.Errorf("no download URL and local fallback %s not found: %w", localPath, err)
		}
		downloadPath = localPath
		log.Printf("No download URL, using local file: %s", localPath)
	}

	// 3. Verify CID — compute CommD via miner API and compare
	unpaddedSize := abi.PaddedPieceSize(evt.Size).Unpadded()

	verifyFile, err := os.Open(downloadPath)
	if err != nil {
		return fmt.Errorf("open file for verification: %w", err)
	}
	pieceInfo, err := minerAPI.ComputeDataCid(ctx, unpaddedSize, verifyFile)
	verifyFile.Close()
	if err != nil {
		return fmt.Errorf("compute data CID: %w", err)
	}
	if !pieceInfo.PieceCID.Equals(pieceCID) {
		return fmt.Errorf("CID mismatch: expected %s, computed %s", pieceCID, pieceInfo.PieceCID)
	}
	if pieceInfo.Size != abi.PaddedPieceSize(evt.Size) {
		return fmt.Errorf("size mismatch: expected %d, computed %d", evt.Size, pieceInfo.Size)
	}
	log.Printf("CID verified: %s", pieceCID)

	// 4. Resolve client actor ID from the DDO contract address.
	// The on-chain allocation is created by the DDO contract (not the user's EOA),
	// so the contract's actor ID must be used in the VerifiedAllocationKey.
	ddoContractAddr, err := address.NewFromString(cfg.DDOContractF4)
	if err != nil {
		return fmt.Errorf("parse DDO contract f4 address %q: %w", cfg.DDOContractF4, err)
	}
	ddoF0Addr, err := fullNode.StateLookupID(ctx, ddoContractAddr, types.EmptyTSK)
	if err != nil {
		return fmt.Errorf("lookup ID for DDO contract %s: %w", ddoContractAddr, err)
	}
	ddoActorIDUint, err := address.IDFromAddress(ddoF0Addr)
	if err != nil {
		return fmt.Errorf("extract actor ID from %s: %w", ddoF0Addr, err)
	}
	clientActorID := abi.ActorID(ddoActorIDUint)

	// 5. CBOR-encode allocation ID as notification payload
	// The DDO contract's readUInt64 expects a CBOR-encoded uint64.
	cborAllocID := cbg.CborInt(evt.AllocationID)
	payload, err := cborutil.Dump(&cborAllocID)
	if err != nil {
		return fmt.Errorf("CBOR-encode allocation ID: %w", err)
	}

	// 6. Get current chain head for deal schedule epochs
	head, err := fullNode.ChainHead(ctx)
	if err != nil {
		return fmt.Errorf("get chain head: %w", err)
	}

	// 7. Open file for streaming to miner (ReaderParamEncoder streams this via HTTP)
	// Zero-pad file data to exactly unpaddedSize bytes (matching ComputeDataCid behavior).
	dataFile, err := os.Open(downloadPath)
	if err != nil {
		return fmt.Errorf("open file for onboarding: %w", err)
	}
	defer dataFile.Close()

	fileStat, err := dataFile.Stat()
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	fileSize := fileStat.Size()
	var dataReader io.Reader
	if fileSize >= int64(unpaddedSize) {
		dataReader = io.LimitReader(dataFile, int64(unpaddedSize))
	} else {
		// Pad with zeros to fill unpaddedSize
		zeroPad := make([]byte, int64(unpaddedSize)-fileSize)
		dataReader = io.MultiReader(dataFile, bytes.NewReader(zeroPad))
	}

	// 8. Onboard piece via SectorAddPieceToAny (DDO path: DealID=0, PAM set)
	so, err := minerAPI.SectorAddPieceToAny(ctx, unpaddedSize, dataReader, piece.PieceDealInfo{
		PublishCid:   nil,
		DealID:       0,
		DealProposal: nil,
		DealSchedule: piece.DealSchedule{
			StartEpoch: head.Height() + abi.ChainEpoch(cfg.StartEpochBuffer),
			EndEpoch:   head.Height() + abi.ChainEpoch(cfg.EndEpochBuffer),
		},
		KeepUnsealed: true,
		PieceActivationManifest: &minertypes.PieceActivationManifest{
			CID:  pieceCID,
			Size: abi.PaddedPieceSize(evt.Size),
			VerifiedAllocationKey: &minertypes.VerifiedAllocationKey{
				Client: clientActorID,
				ID:     verifregtypes.AllocationId(evt.AllocationID),
			},
			Notify: []minertypes.DataActivationNotification{
				{
					Address: ddoContractAddr,
					Payload: payload,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("SectorAddPieceToAny: %w", err)
	}

	log.Printf("Piece added to sector %d at offset %d", so.Sector, so.Offset)
	return nil
}

// resolveClientActorID converts an Ethereum address to a Filecoin actor ID.
// eth addr → EthAddress.ToFilecoinAddress() (f4) → StateLookupID (f0) → IDFromAddress (uint64)
func resolveClientActorID(ctx context.Context, fullNode lapi.FullNode, clientEthAddr common.Address) (abi.ActorID, error) {
	var ethAddr ethtypes.EthAddress
	copy(ethAddr[:], clientEthAddr[:])

	f4Addr, err := ethAddr.ToFilecoinAddress()
	if err != nil {
		return 0, fmt.Errorf("eth addr to f4: %w", err)
	}

	f0Addr, err := fullNode.StateLookupID(ctx, f4Addr, types.EmptyTSK)
	if err != nil {
		return 0, fmt.Errorf("lookup ID for %s: %w", f4Addr, err)
	}

	id, err := address.IDFromAddress(f0Addr)
	if err != nil {
		return 0, fmt.Errorf("extract actor ID from %s: %w", f0Addr, err)
	}

	return abi.ActorID(id), nil
}

// downloadFile fetches a URL and writes it to destPath.
func downloadFile(ctx context.Context, url, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}
