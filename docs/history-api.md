# History API (1.2.5-LTS1)

## Bounded chart query

`POST /api/v1/history/query` accepts load and Ping history requests. The server
always applies a 20-second deadline and limits the complete response to at most
5,000 points. `max_points` defaults to a response-wide budget of 1,500.

```json
{
  "type": "ping",
  "uuid": "client-uuid",
  "task_id": 1,
  "start": "2026-07-01T00:00:00Z",
  "end": "2026-07-16T00:00:00Z",
  "max_points": 1500
}
```

`type` is `ping` or `load`. A request may use `hours` instead of `start` and
`end`. The maximum online range is 90 days. Ping points contain `avg`, `min`,
`max`, `loss_count`, and `total_count`. Load and GPU points contain their
averaged fields in `metrics`. The response metadata reports `resolution`,
`raw_count`, `returned_points`, and `sampled`.

Themes can use the cancellation-aware browser client served by Komari:

```js
import { KomariHistoryClient } from "/api/v1/history/client.js";

const history = new KomariHistoryClient();
const result = await history.query(request, {
  key: `ping:${uuid}:${taskId}`,
  timeout: 25000,
});

// Call when changing range or unmounting the chart.
history.cancel(`ping:${uuid}:${taskId}`);
```

The legacy `/api/records/load` and `/api/records/ping` routes use the same
bounded backend, so themes can migrate independently without retaining the old
unbounded query behavior.

The pinned `komari-web/1.2.5-fix2` default theme uses RPC2. Its
`public:queryMetrics`, `public:getPingMetricStats`, and
`public:listMetricDefinitions` methods are compatibility adapters over this
same history service; they do not introduce the later metrics database.

## Raw CSV export

Raw export is administrator-only and asynchronous:

```text
POST   /api/admin/history/export
GET    /api/admin/history/export/:id
GET    /api/admin/history/export/:id/download
DELETE /api/admin/history/export/:id
```

The POST body uses the same range and target fields as the chart query. One
worker processes 15-minute windows and releases the read connection between
windows. Ping exports contain every raw `ping_records` row. Load exports add a
`source` column to distinguish `records` from `records_long_term`. Completed
files expire after 48 hours.

## Error handling

- HTTP 499: the caller cancelled the request.
- HTTP 504: the server-side history deadline expired.
- HTTP 503: persistence or export queue is temporarily unavailable.
- HTTP 400: invalid target, range, or parameters.
