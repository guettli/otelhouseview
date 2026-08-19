# otelhouseview

The **read path** for OpenTelemetry data (logs, metrics, traces) stored in
ClickHouse by the OpenTelemetry Collector's
[clickhouseexporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter),
whose stock schema this repo targets (`otel_logs`, `otel_traces`,
`otel_metrics_{gauge,sum,histogram,exponential_histogram,summary}`).
The write path is [otelhouse](https://github.com/guettli/otelhouse).

otelhouseview is a **library, not a service**. It ships **three** Go packages:

| Package | What it is |
| ------- | ---------- |
| [`otelstore`](otelstore) | Read-only, typed ClickHouse client for that schema (`GetTrace`, `ListTraces`, + an in-memory fake). |
| [`ciview`](ciview) | Renders a trace as a self-contained HTML page: Gantt waterfall + span tree + logs. |
| [`explore`](explore) | The **SQL workbench** as an embeddable sub-application: one `http.Handler` serving its own JSON API *and* its own embedded Svelte SPA, mountable under any prefix. |

Plus `cmd/otelhouseview`, a standalone binary that mounts `explore` at `/` —
useful for local dev, **not** the production shape (see below).

## The usage pattern

A **host application** imports these packages, supplies a **tenant-scoped
ClickHouse DSN**, and mounts `explore` behind **its own** authentication:

```go
import "github.com/guettli/otelhouseview/explore"

svc, err := explore.New(ctx, explore.Config{
    DSN:       os.Getenv("CLICKHOUSE_DSN"), // <ns>_ro — the security boundary
    StorePath: "/data/explore.db",          // saved queries; caller owns the path
    Prefix:    "/explore",                  // where the host mounts it
})
defer svc.Close()

// The host authenticates. explore does not.
mux.Handle("/explore/", app.RequireSession(http.StripPrefix("/explore", svc.Handler())))
```

Today's host is [agentloop](https://github.com/guettli/agentloop): it already
has passkey/WebAuthn sessions, an embedded SPA, an ingress, a TLS cert and a
per-tenant `CLICKHOUSE_DSN`. Mounting the workbench inside it at `/explore` is
strictly less machinery than deploying a second service per tenant and building
all of that a second time.

## Safety model: the DSN is the boundary

`explore` **does no authentication and enforces no tenancy — by construction.**
It serves an arbitrary-SQL endpoint; mounted naked on a public listener it is a
SQL console for the internet. The host MUST wrap it.

What makes it safe to hand a tenant arbitrary SQL is not anything in this repo,
it is the **ClickHouse identity in the DSN**:

- The DSN is a per-tenant `<tenant>_ro` user whose **row policies** are pinned to
  `ResourceAttributes['tenant'] = '<tenant>'` — so no SQL, however creative, can
  read another tenant's rows. Tenancy is established at write time by the
  [otelhouse](https://github.com/guettli/otelhouse) gateway, fail-closed.
- That user's **server-side settings profile** caps query cost
  (`max_execution_time`, `max_rows_to_read`, `max_bytes_to_read`,
  `max_result_rows`, `max_memory_usage`) — so no query can outrun its budget.

Consequently this repo adds **no** tenant predicate and **no** Go-side limits.
Either would *look* like a security boundary without being one, and would drift
from the real one. Saved-query params bind via ClickHouse native parameters
(`{name:Type}`) — never string interpolation.

## What the workbench does

- Run ClickHouse SQL against the OTel tables and get a result grid.
- Automatically render a time-series chart when a result looks like `(time, [group], value)`.
- Save a query as a **named, parameterized template**; re-run it deterministically
  from a simple form — no SQL, no LLM.
- Ships a starter library of queries for the exporter schema.
- Renders a trace as a waterfall (that is `ciview`, shipped and exported).

### Mounting details

- **The host strips the prefix** (`http.StripPrefix`). `Config.Prefix` is used
  only to generate correct absolute URLs (assets, API base) in the served
  `index.html`. The handler also tolerates the un-stripped case, so a host that
  mounts `svc.Handler()` directly still works — but `Config.Prefix` must match
  the real mount point either way.
- The SPA build (`explore/web/build/`) is **committed**, because `go:embed`
  ships whatever is in the module and importers do not run pnpm. Re-run
  `make web` and commit after touching `explore/web/src`.

## Go library: `otelstore` + `ciview`

The read core is importable — so consumers do not each grow their own copy of
"SELECT spans out of the clickhouseexporter schema":

```go
import "github.com/guettli/otelhouseview/otelstore"

store, err := otelstore.OpenClickHouse(ctx, dsn) // dsn = a read-only CH user
trace, err := store.GetTrace(ctx, traceID)       // spans + logs, one trace
recent, err := store.ListTraces(ctx, 50)         // newest-first summaries

// ...or narrow the listing to one kind of producer. ResourceAttrKeys matches
// on key *presence*, so "traces from a CI pipeline" needs no value list:
ci, err := store.ListTracesFiltered(ctx, otelstore.ListOptions{
    Limit:            50,
    ResourceAttrKeys: []string{"ci.commit"},
    Since:            time.Now().Add(-14 * 24 * time.Hour),
})
```

A listing is assembled per trace, not by selecting the span whose parent is
empty. A trace whose parent context was minted outside the store — what a CI
workflow produces when it injects its own `TRACEPARENT` — has no such span,
and is listed all the same, with its earliest span standing in for the root
and its duration spanning the whole run.

`MemoryStore` is the in-process fake, so consumers can test their rendering
without a live ClickHouse.

`ciview` renders a trace as a self-contained HTML page — a Gantt waterfall, a
collapsible span tree, and the logs emitted under each span, with no backend
and no framework. Mount the bytes on any route:

```go
import "github.com/guettli/otelhouseview/ciview"

page, err := ciview.RenderTrace(trace)        // one trace, full waterfall
index, err := ciview.RenderIndex(recent)      // listing of recent traces
```

### These packages have external consumers

`otelstore/`, `ciview/` and `explore/` are **exported API with out-of-repo
importers**:

| Consumer | Imports |
| -------- | ------- |
| [agentloop](https://github.com/guettli/agentloop) | `otelstore` + `ciview` + `explore` |
| [otelhouse](https://github.com/guettli/otelhouse) e2e harness | `otelstore` |

They pin untagged pseudo-versions, so a breaking change to `Store`, `GetTrace`,
`ListTraces`, `RenderTrace`, `RenderIndex` or `explore.New` / `Config` /
`Handler` / `Close` (signatures, or the `Span` / `LogRecord` / `Trace` /
`TraceSummary` shapes they carry) breaks them on their next `go get -u`. Change
these deliberately, additively where possible. Everything under `internal/` —
including `explore/internal/` — is private and free to churn.

### Options and the `DB()` escape hatch

```go
store, err := otelstore.OpenClickHouse(ctx, dsn,
    otelstore.WithTracesTable("otel_traces"), // only if the exporter was
    otelstore.WithLogsTable("otel_logs"),     // configured off its defaults
)

db := store.DB() // *sql.DB, owned by the store — do NOT close it
```

`DB()` exposes the underlying `database/sql` handle so a caller can run its own
read-only queries (e.g. aggregating `otel_metrics_sum`) without this package
having to grow an API for them.

**Sharp edge — `GetTrace` is span-anchored.** It reports `ErrNotFound` when the
**spans** query comes back empty, even if the trace id has log rows. A trace
that produced only logs (or whose spans have not landed yet) therefore looks
absent. This is deliberate — the viewer renders a waterfall and has nothing to
hang logs on without spans — but it means "did my data arrive?" is not a
question `GetTrace` can answer. otelhouse's e2e asserts log ingestion through
`DB()` for exactly this reason.

### Tenancy: the library is deliberately tenant-blind

In a multi-tenant deployment the isolation boundary is the ClickHouse identity
in the DSN — the [otelhouse](https://github.com/guettli/otelhouse) gateway
stamps `ResourceAttributes['tenant']` at write time, and reads are constrained
by a row policy bound to a per-tenant `<tenant>_ro` read-only user. No query
here adds a tenant predicate, and none should: a filter in Go would *look* like
a security boundary without being one. Callers pass a DSN already scoped to one
tenant.

## Roadmap (not in v1)

- AI-assisted query authoring (agent restricted to a read-only ClickHouse MCP tool).
- Multi-chart dashboards.

## Deployment shape

There is **no standalone otelhouseview service per tenant, and there will not
be.** The host app (agentloop) already owns auth, an SPA, an ingress, a TLS cert
and the per-tenant DSN; a second deployment would rebuild all of it. So:

- `cmd/otelhouseview` exists for **local development** — one process, `PORT` /
  `CLICKHOUSE_DSN` / `SQLITE_PATH` from the environment, workbench at `/`. It
  authenticates nobody. Do not put it on a public listener.
- The public static-report host (`otelhouseview.thomas-guettler.de`) is **gone**;
  the report below stays as a CI artifact.

## Static report pipeline (CI)

Alongside the workbench, otelhouseview ships a **static report** path: a
self-contained HTML report (Svelte + Vite, `ui/`) with no live backend. It is
rendered from **synthetic** telemetry that `ci/cmd/emit` generates seconds
earlier — it has never shown a real span, and it is not deployed anywhere. Its
value is the **end-to-end coverage**, not the page. The Dagger CI pipeline
(`ci/`) is the single source of truth and, end to end:

1. runs an **ephemeral ClickHouse** and the **upstream OTel Collector**;
2. runs `ci/cmd/emit` to push spread **logs, metrics and traces** over OTLP;
3. runs `ci/cmd/genreport` to query ClickHouse and write `report.json`;
4. bakes that JSON into a single `dist/index.html` via the Svelte build;
5. attaches the rendered page to the CI run as an artifact.

The report used to be uploaded into a ConfigMap and served from a public host.
That was retired: otelhouse's e2e now asserts the same pipeline **in code**,
which is a stronger check than a human looking at a page of made-up data.

`make ci` runs exactly what GitHub Actions runs (needs a reachable Dagger
engine). `make ui-build` builds the report locally against the committed sample
data. The report renders offline — it is one HTML file with everything inlined.

See [docs/DESIGN.md](docs/DESIGN.md) for the architecture and the decisions behind it.
