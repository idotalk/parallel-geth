# Block Parallelizability & Sequential Tail Characterization Report

**Execution Parameters**: Direct Execution Max Wave Size $M = 9$

## Block Summary Table

| Block Number | File | Total Txs ($N$) | Wave Count ($K$) | Max Wave | Avg Wave | Structural Par ($|W_k| \ge 2$) | Threshold Par ($|W_k| > M$) | Tail Txs ($N_{\text{tail}}$) | Tail % | Conflict Density |
|---|---|---|---|---|---|---|---|---|---|---|
| 25603091 | `25603091.json` | 442 | 86 | 171 | 5.14 | 96.6% | 53.2% | 207 | 46.8% | 0.0608 |
| 25603092 | `25603092.json` | 469 | 134 | 168 | 3.50 | 85.9% | 51.6% | 227 | 48.4% | 0.1019 |
| 25603093 | `25603093.json` | 172 | 65 | 54 | 2.65 | 82.0% | 31.4% | 118 | 68.6% | 0.1544 |
| 25603094 | `25603094.json` | 199 | 59 | 55 | 3.37 | 92.0% | 34.7% | 130 | 65.3% | 0.1277 |
| 25603095 | `25603095.json` | 118 | 32 | 39 | 3.69 | 94.1% | 41.5% | 69 | 58.5% | 0.1058 |
| 25603096 | `25603096.json` | 623 | 165 | 211 | 3.78 | 92.0% | 42.1% | 361 | 57.9% | 0.0910 |
| 25603097 | `25603097.json` | 224 | 38 | 83 | 5.89 | 98.7% | 44.2% | 125 | 55.8% | 0.0734 |
| 25603098 | `25603098.json` | 351 | 94 | 136 | 3.73 | 91.5% | 47.9% | 183 | 52.1% | 0.0968 |
| 25603099 | `25603099.json` | 92 | 25 | 20 | 3.68 | 98.9% | 21.7% | 72 | 78.3% | 0.1598 |
| 25603100 | `25603100.json` | 235 | 87 | 61 | 2.70 | 79.6% | 39.6% | 142 | 60.4% | 0.1282 |

## Key Definitions & Insights

- **Structural Parallelism ($|W_k| \ge 2$)**: Transactions in waves containing multiple transactions.
- **Threshold-Aware Parallelism ($|W_k| > 9$)**: Transactions executed concurrently under direct execution threshold $M=9$.
- **Sequential Tail ($N_{\text{tail}}$)**: Suffix waves following the final parallel wave ($|W_k| > 9$) in the block.
- **Conflict Density**: Pairwise transaction account overlap ratio $\frac{2E}{N(N-1)}$.
