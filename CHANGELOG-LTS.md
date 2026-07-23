# Komari 1.2.5-LTS1

- Added a bounded, cancellable history service shared by load, GPU, and Ping charts, with a response-wide point budget.
- Kept legacy history routes compatible while moving their queries to the bounded backend.
- Added bounded RPC2 metric adapters required by the pinned 1.2.5-fix2 default frontend, without adding a new metrics database.
- Added a standard browser client with request replacement, cancellation, and timeout handling.
- Added the missing task/time Ping index while retaining the existing client/time index.
- Isolated SQLite history reads from the single legacy write connection under WAL mode.
- Serialized foreground SQLite writes with bounded busy retries and write-priority maintenance scheduling.
- Added durable load and Ping ingest spools so transient lock pressure does not create silent history gaps.
- Changed retention cleanup to small batches and record compaction to one 15-minute window per pass.
- Added administrator-only asynchronous raw CSV exports for Ping and resource history.
- Reused the existing release workflows, pinned the default frontend to `komari-web/1.2.5-fix2`, and added GitHub-side Go formatting and test gates.
