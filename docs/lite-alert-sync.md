# Lite alert routing and digest

Ported behavior and regression fixtures from MIT-licensed nuomiiiii/Lite a73311d35f06d5f8f7a33937856dc485b88bfc80 (notably 15a2cc0 and 059a344). Retains Komari's managed registry, moejs channels and admin-only RPC2 boundaries.

Each alert kind selects zero or more channel IDs. The old selection is migrated once; an explicitly empty table stays empty. A saved plugin selection is retained even before plugins load. Registered channels share routing and manual targeted tests with built-ins; registry configurations resolve on delivery. Unloaded plugin routes may be retained or removed, but cannot be newly selected.

Digest defaults off with a 5 second fixed window, configurable to integer 1–3600 seconds. Identical kind/event/message/emoji batches deduplicate node IDs; differing content stays separate. Login and scheduled reports send immediately. Enqueue is nonblocking. Flush rereads the master switch and current routes, so pending events respect later disabling. Pending batches live in memory and flush at orderly message-sender shutdown; a crash may lose pending alerts. Provider failures are logged per channel; other selected channels are still attempted.

Compared with upstream, avoids unused per-event waiter channels and serializes provider replacement against delivery. No local builds or tests; Actions race tests and full logs retained 30 days. No E2E.
