#!/usr/bin/env python3
"""Fetch a mainnet block plus exact parent prestate for real-execution benchmarks.

Uses debug_traceBlockByNumber + prestateTracer (one call) and merges per-tx
prestates with first-wins ordering so `pre` is state before the block.

Emits all executable tx types (legacy, access-list, dynamic-fee, blob, setcode).
Blob txs are included without sidecars (valid in-block form for geth).

Usage:
  python benchmarks/blocks/fetch_block_with_prestate.py
  python benchmarks/blocks/fetch_block_with_prestate.py --block 25560620
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

# ---------------------------------------------------------------------------
# CONFIG
# ---------------------------------------------------------------------------

RPC_URI = "https://docs-demo.quiknode.pro/"
OUTPUT_DIR = Path(__file__).resolve().parent / "blocksdata"
REQUEST_RETRIES = 4
RETRY_BASE_SLEEP_S = 2.0
MIN_REQUEST_INTERVAL_S = 0.3

# ---------------------------------------------------------------------------

_TRANSIENT = (
    ConnectionResetError,
    ConnectionAbortedError,
    TimeoutError,
    urllib.error.URLError,
    urllib.error.HTTPError,
)

_last_request_at = 0.0


def _throttle() -> None:
    global _last_request_at
    now = time.monotonic()
    wait = MIN_REQUEST_INTERVAL_S - (now - _last_request_at)
    if wait > 0:
        time.sleep(wait)
    _last_request_at = time.monotonic()


def _post_json(payload: Any, timeout: float = 600) -> Any:
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        RPC_URI,
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    last_err: Exception | None = None
    for attempt in range(REQUEST_RETRIES):
        _throttle()
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return json.loads(resp.read().decode())
        except _TRANSIENT as err:
            last_err = err
            sleep_s = RETRY_BASE_SLEEP_S * (2**attempt)
            print(f"  RPC retry {attempt + 1}/{REQUEST_RETRIES} after {err!r}; sleep {sleep_s:.1f}s")
            time.sleep(sleep_s)
    assert last_err is not None
    raise last_err


def rpc(method: str, params: list[Any]) -> Any:
    payload = _post_json({"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
    if "error" in payload:
        raise RuntimeError(f"{method}: {payload['error']}")
    return payload["result"]


def parse_block_arg(value: str) -> str:
    value = value.strip().lower()
    if value in ("latest", "pending", "earliest", "safe", "finalized"):
        return value
    if value.startswith("0x"):
        return value
    return hex(int(value))


def hex_to_int(value: str) -> int:
    return int(value, 16)


def normalize_addr(addr: str) -> str:
    return "0x" + addr.lower().removeprefix("0x").zfill(40)


def format_tx(tx: dict[str, Any]) -> dict[str, Any]:
    """Keep original signed fields for every tx type (including blobs, no sidecar)."""
    tx_type = hex_to_int(tx.get("type") or "0x0")
    formatted: dict[str, Any] = {
        "type": hex(tx_type),
        "accessList": tx.get("accessList") or [],
        "data": tx.get("input") or "0x",
        "gasLimit": tx["gas"],
        "nonce": tx["nonce"],
        "to": tx.get("to") or "",
        "value": tx.get("value") or "0x0",
        "v": tx["v"],
        "r": tx["r"],
        "s": tx["s"],
        "from": tx.get("from"),
    }
    if tx.get("gasPrice") is not None:
        formatted["gasPrice"] = tx["gasPrice"]
    if tx.get("maxFeePerGas") is not None:
        formatted["maxFeePerGas"] = tx["maxFeePerGas"]
    if tx.get("maxPriorityFeePerGas") is not None:
        formatted["maxPriorityFeePerGas"] = tx["maxPriorityFeePerGas"]
    if tx.get("maxFeePerBlobGas") is not None:
        formatted["maxFeePerBlobGas"] = tx["maxFeePerBlobGas"]
    if tx.get("blobVersionedHashes"):
        formatted["blobVersionedHashes"] = tx["blobVersionedHashes"]
    if tx.get("authorizationList"):
        formatted["authorizationList"] = tx["authorizationList"]
    if tx.get("chainId") is not None:
        formatted["chainId"] = tx["chainId"]
    return formatted


def merge_prestate(traces: list[Any]) -> dict[str, Any]:
    """Merge per-tx prestates in block order; first touch wins (= parent state)."""
    pre: dict[str, Any] = {}
    for entry in traces:
        result = entry.get("result", entry) if isinstance(entry, dict) else entry
        if not isinstance(result, dict):
            continue
        for addr, acct in result.items():
            if not isinstance(acct, dict):
                continue
            key = normalize_addr(addr)
            if key not in pre:
                account: dict[str, Any] = {
                    "balance": acct.get("balance") or "0x0",
                    "nonce": acct.get("nonce") or "0x0",
                }
                code = acct.get("code") or "0x"
                if code and code != "0x":
                    account["code"] = code
                storage = acct.get("storage") or {}
                if storage:
                    account["storage"] = {
                        ("0x" + k.lower().removeprefix("0x").zfill(64)): v
                        for k, v in storage.items()
                    }
                pre[key] = account
                continue
            existing = pre[key]
            if not existing.get("code"):
                code = acct.get("code") or "0x"
                if code and code != "0x":
                    existing["code"] = code
            slots = acct.get("storage") or {}
            if slots:
                dest = existing.setdefault("storage", {})
                for sk, sv in slots.items():
                    nk = "0x" + sk.lower().removeprefix("0x").zfill(64)
                    if nk not in dest:
                        dest[nk] = sv
    return pre


def header_from_block(block: dict[str, Any]) -> dict[str, Any]:
    header = {
        "baseFeePerGas": block.get("baseFeePerGas"),
        "bloom": block.get("logsBloom"),
        "coinbase": block.get("miner"),
        "difficulty": block.get("difficulty"),
        "extraData": block.get("extraData"),
        "gasLimit": block.get("gasLimit"),
        "gasUsed": block.get("gasUsed"),
        "mixHash": block.get("mixHash"),
        "nonce": block.get("nonce"),
        "number": block.get("number"),
        "parentHash": block.get("parentHash"),
        "receiptTrie": block.get("receiptsRoot"),
        "stateRoot": block.get("stateRoot"),
        "timestamp": block.get("timestamp"),
        "transactionTrie": block.get("transactionsRoot"),
        "uncleHash": block.get("sha3Uncles"),
    }
    return {k: v for k, v in header.items() if v is not None}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--block", default="latest", help='Decimal, hex, or "latest"')
    parser.add_argument("--out-dir", type=Path, default=OUTPUT_DIR)
    args = parser.parse_args()

    block_tag = parse_block_arg(args.block)
    print(f"Fetching block {block_tag} from {RPC_URI}")
    block = rpc("eth_getBlockByNumber", [block_tag, True])
    if not block:
        print("Block not found", file=sys.stderr)
        return 1

    block_num = hex_to_int(block["number"])
    txs = block["transactions"]
    print(f"Block {block_num}: {len(txs)} transactions")

    print("Tracing prestate (debug_traceBlockByNumber + prestateTracer)...")
    traces = rpc(
        "debug_traceBlockByNumber",
        [block["number"], {"tracer": "prestateTracer", "tracerConfig": {"diffMode": False}}],
    )
    if not isinstance(traces, list) or not traces:
        print(f"Unexpected trace result: {type(traces).__name__}", file=sys.stderr)
        return 1
    pre = merge_prestate(traces)
    print(f"  merged prestate from {len(traces)} tx traces")

    formatted_txs = [format_tx(tx) for tx in txs]
    type_counts: dict[int, int] = {}
    for tx in formatted_txs:
        t = hex_to_int(tx["type"])
        type_counts[t] = type_counts.get(t, 0) + 1

    header = header_from_block(block)
    name = f"bcLiveGeneratedBlock_{block_num}"
    payload = {
        name: {
            "pre": pre,
            "blockHeader": header,
            "blocks": [
                {
                    "blockHeader": header,
                    "transactions": formatted_txs,
                    "uncleHeaders": [],
                }
            ],
        }
    }

    args.out_dir.mkdir(parents=True, exist_ok=True)
    out_path = args.out_dir / f"{block_num}.json"
    out_path.write_text(json.dumps(payload, indent=4), encoding="utf-8")

    code_accounts = sum(1 for a in pre.values() if a.get("code"))
    slot_count = sum(len(a.get("storage") or {}) for a in pre.values())
    print(f"Wrote {out_path}")
    print(
        f"pre: {len(pre)} accounts, {code_accounts} with code, {slot_count} slots; "
        f"txs={len(formatted_txs)} types={dict(sorted(type_counts.items()))}"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except _TRANSIENT as err:
        print(f"RPC request failed: {err}", file=sys.stderr)
        raise SystemExit(1) from err
    except RuntimeError as err:
        print(f"RPC error: {err}", file=sys.stderr)
        raise SystemExit(1) from err
