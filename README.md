# Lotus DDO Plugin

A lightweight Go service that automates **Filecoin Direct Data Onboarding (DDO)** for storage providers running Lotus. It polls a DDO smart contract for `AllocationCreated` events and automatically onboards matching pieces into the Lotus miner's sealing pipeline.

## How It Works

```
DDO Contract emits AllocationCreated event
        │
        ▼
Plugin polls for new events (filtered by PROVIDER_ID)
        │
        ▼
Downloads piece data from the event's download URL
        │
        ▼
Verifies piece CID via Lotus miner's ComputeDataCid
        │
        ▼
Calls SectorAddPieceToAny with PieceActivationManifest (DDO path)
        │
        ▼
Lotus sealing pipeline takes over (PreCommit → Commit → Prove)
```

The plugin uses the DDO path (`DealID=0`) with a `PieceActivationManifest` containing the verified allocation key and a notification payload so the DDO contract is called back when the sector activates on-chain.

## Prerequisites

- **Lotus full node** — synced and accessible via API
- **Lotus miner** — running and accessible via API, with sealing capacity available
- **Go 1.22+** — for building from source
- **Local Lotus checkout** — required at `../lotus` with FFI submodule initialized (see `go.mod` replace directives)

## Build

```bash
go build -o lotus-ddo-plugin .
```

## Configuration

Copy the example env file and fill in your values:

```bash
cp .env.example .env
```

### Required Variables

| Variable | Description |
|---|---|
| `RPC_URL` | Ethereum-compatible RPC endpoint (for polling contract events) |
| `CONTRACT_ADDRESS` | DDO contract address (0x format) |
| `PROVIDER_ID` | Your miner's actor ID (numeric, e.g. `1000`) |
| `LOTUS_API_URL` | Lotus full node API endpoint |
| `LOTUS_API_TOKEN` | Lotus full node auth token (needs read access) |
| `MINER_API_URL` | Lotus miner API endpoint |
| `MINER_API_TOKEN` | Lotus miner auth token (needs admin access for `ComputeDataCid` and `SectorAddPieceToAny`) |
| `DDO_CONTRACT_F4` | DDO contract's Filecoin f4 address (e.g. `f410f...`). Used to resolve the client actor ID for the verified allocation key. |

### Optional Variables

| Variable | Default | Description |
|---|---|---|
| `POLL_INTERVAL` | `30s` | How often to poll for new events |
| `START_BLOCK` | `latest` | Block number to start scanning from, or `latest` |
| `DOWNLOAD_DIR` | `./downloads` | Temp directory for downloaded piece data |
| `START_EPOCH_BUFFER` | `5760` (~2 days) | Epochs after chain head for deal start |
| `END_EPOCH_BUFFER` | `1152000` (~400 days) | Epochs after chain head for deal end |

### Generating API Tokens

```bash
# Lotus full node token (read access)
lotus auth create-token --perm read

# Lotus miner token (admin access required)
lotus-miner auth create-token --perm admin
```

### Getting the DDO Contract f4 Address

The plugin needs the DDO contract's Filecoin f4 address (not the 0x Ethereum address). You can convert it using the Lotus CLI:

```bash
lotus evm stat <0x-contract-address>
```

Look for the `ID address` and `Robust address` (f4) in the output.

## Network Configuration

### Filecoin Calibration Testnet

| Setting | Value |
|---|---|
| **RPC URL** | `https://api.calibration.node.glif.io/rpc/v1` |
| **DDO Contract** | `0x889fD50196BE300D06dc4b8F0F17fdB0af587095` |

### Filecoin Mainnet

| Setting | Value |
|---|---|
| **RPC URL** | `https://api.node.glif.io/rpc/v1` |
| **DDO Contract** | `0x94A53ac3ca6743990ebB659F3Fe84198420d088c` |

> **Note:** You can use any Filecoin-compatible RPC endpoint. The Glif URLs above are public endpoints. For production use, consider running your own Lotus node or using a dedicated RPC provider.

## Running

```bash
# With .env file in the current directory
./lotus-ddo-plugin

# Or with env vars exported directly
RPC_URL=wss://... CONTRACT_ADDRESS=0x... ./lotus-ddo-plugin
```

The plugin will:
1. Connect to the Ethereum RPC, Lotus full node, and Lotus miner APIs
2. Start polling from the configured block (or latest)
3. For each `AllocationCreated` event matching your `PROVIDER_ID`:
   - Download the piece data from the event's `downloadURL`
   - Verify the piece CID matches via `ComputeDataCid`
   - Submit the piece to the miner via `SectorAddPieceToAny`
   - Clean up the downloaded file

### Example .env for Calibration Testnet

```bash
RPC_URL=https://api.calibration.node.glif.io/rpc/v1
CONTRACT_ADDRESS=0x889fD50196BE300D06dc4b8F0F17fdB0af587095
PROVIDER_ID=17840

LOTUS_API_URL=ws://localhost:1234/rpc/v1
LOTUS_API_TOKEN=eyJ...
MINER_API_URL=ws://localhost:2345/rpc/v0
MINER_API_TOKEN=eyJ...

DDO_CONTRACT_F4=f410f...

POLL_INTERVAL=30s
START_BLOCK=latest
```

## Architecture

Three source files, one package (`main`):

- **`main.go`** — Config loading, client initialization, block-polling loop. Connects to Ethereum RPC (event logs), Lotus full node (chain state), and Lotus miner (piece computation and sector onboarding).

- **`event.go`** — Parses `AllocationCreated` events from Ethereum logs. Indexed fields (client, allocationId, provider) come from log topics; non-indexed fields (data, size, terms, downloadURL) are ABI-decoded from log data.

- **`onboard.go`** — Handles the full onboarding pipeline for a single allocation: download → verify CID → resolve actor ID → CBOR-encode notification → `SectorAddPieceToAny` with `PieceActivationManifest`.

## Known Limitations

- **No retry logic** — If an allocation fails (download error, CID mismatch, miner API failure), the block cursor advances past it. The failed allocation will not be retried automatically.
- **No persistence** — The last processed block is held in memory only. If the process restarts, it resumes from `START_BLOCK` (or `latest`), potentially missing events emitted during downtime unless `START_BLOCK` is set to the last known block.
