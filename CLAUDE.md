# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build
go build -o lotus-ddo-plugin .

# Run (requires .env or exported env vars)
./lotus-ddo-plugin
```

There are no tests in this repo currently. Configuration is via `.env` file (see `.env.example`).

## Local Dependency Setup

The `go.mod` uses `replace` directives pointing to a sibling `../lotus` checkout:
```
replace github.com/filecoin-project/lotus => ../lotus
replace github.com/filecoin-project/filecoin-ffi => ../lotus/extern/filecoin-ffi
```
You must have a compatible Lotus repo at `../lotus` with its FFI submodule initialized.

## Architecture

This is a single-binary Go service that automates Filecoin Direct Data Onboarding (DDO). It polls an FEVM smart contract for allocation events and onboards matching pieces into a Lotus storage provider.

**Three source files, one package (`main`):**

- **`main.go`** — Config loading from env vars, Ethereum + Lotus client initialization, block-polling loop (`pollEvents`). Connects to three APIs: Ethereum RPC (for event logs), Lotus full node (for chain state/actor resolution), and Lotus miner (for piece computation and sector onboarding).

- **`event.go`** — Parses `AllocationCreated` events from Ethereum logs. ABI definition is inline. Indexed fields (client, allocationId, provider) come from log topics; non-indexed fields (data, size, terms, downloadURL) are ABI-decoded from log data.

- **`onboard.go`** — `processAllocation` handles the full onboarding pipeline for a single event: download piece data → verify CID via `ComputeDataCid` → resolve DDO contract's actor ID → CBOR-encode notification payload → call `SectorAddPieceToAny` with a `PieceActivationManifest` (DDO path: DealID=0).

**Key flow:** The polling loop in `main.go` fetches new blocks, calls `fetchAllocationEvents` (event.go) to get logs, filters by `PROVIDER_ID`, then calls `processAllocation` (onboard.go) for each match.

**DDO-specific detail:** The on-chain allocation is created by the DDO smart contract (not the user's EOA), so client actor ID resolution uses the contract's f4 address (`DDO_CONTRACT_F4`), not the event's `client` field.
