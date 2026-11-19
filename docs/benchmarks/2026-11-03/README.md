# Historical response projection measurements

Environment: disposable Linux/amd64 Docker container, Go 1.25.14, CGO enabled,
`GOMAXPROCS=3`, 3-CPU quota and 5 GiB memory limit; host CPU reported by Go:
13th Gen Intel Core i9-13900HX. These are development-machine measurements, not
production-node results or statistically controlled hardware benchmarks.

Reproduce with `bash scripts/ci/measure-history.sh ci-artifacts/history`.
The `previous` path is the retained original full-point projection; `grouped`
is the current compact index projection for the selected nodes. The fixtures,
time ranges and warm SQLite state are the same, with response caching bypassed.
Each benchmark uses a 200 ms target and three repetitions; the table shows
medians. Full output is retained alongside this file.

| File-backed read + projection (4 nodes, 3 metrics) | Previous time | Indexed time | Previous B/op | Indexed B/op |
| --- | ---: | ---: | ---: | ---: |
| 1h | 0.609 ms | 0.543 ms | 297,759 | 242,366 |
| 24h | 7.148 ms | 7.076 ms | 6,050,166 | 4,968,634 |
| 30d | 85.477 ms | 88.548 ms | 11,360,589 | 9,984,346 |

Allocation volume falls about 18% / 18% / 12% in these fixtures. The 30d timing
is about 3.6% slower here, so this is retained as an allocation reduction, not a
claim that every query is faster. Allocation *counts* increase slightly because
small index slices replace large point slices; see raw `allocs/op` values.

The isolated 32-node/500-point projection benchmark falls from 41,156,630 B/op
to 264,616 B/op and from 10.014 ms to 0.349 ms. This isolates response assembly;
it must not be advertised as a corresponding database-wide speedup.

Sequential separate-process load experiments used two readers, twelve mixed
1h/24h/30d queries and ongoing ingestion:

| Observation | Previous | Indexed |
| --- | ---: | ---: |
| Query p50 / p95 | 221.24 / 246.96 ms | 202.44 / 230.13 ms |
| Ingestion p50 / p95 | 0.323 / 0.611 ms | 0.313 / 0.745 ms |
| Peak sampled Go heap | 14,158,504 B | 13,197,608 B |
| Process RSS high water | 71,372 KiB | 71,304 KiB |

Ingestion continued successfully and stayed below 1 ms at p95 in this run; its
tail was somewhat higher in the indexed sample. Query latency and RSS are noisy,
and RSS includes fixture construction. These single load observations do not
prove a throughput gain, a general RSS saving, or a bound on production memory.
Both modes passed cancellation and subsequent read-slot recovery. No storage
schema, percentile algorithm, ingestion path or database index changed.
