# otelhouseview — design

## Goal

Query + visualize OTel data in ClickHouse (clickhouseexporter schema) — and
expose that read core as a Go library other repos import. AI assistance is a
later layer, not on the v1 path.

otelhouseview is a **library, not a service**. It ships three packages —
`otelstore` (typed read client), `ciview` (trace waterfall renderer) and
`explore` (the embeddable SQL workbench) — plus a standalone binary that is a
development convenience, not the production shape.

Each instance serves **one tenant**: it is pointed at a single ClickHouse
read-only identity and has no tenant switcher, no per-request tenant, no
cross-tenant view. That is a property of the *DSN it is handed*, not of the data
— see [Tenancy](#tenancy).

## The usage pattern

```
      host app (agentloop)                     otelhouseview
 ┌──────────────────────────────┐        ┌──────────────────────┐
 │ passkey/WebAuthn session     │        │                      │
 │ ingress + TLS                │        │  explore.New(Config{ │
 │ per-tenant CLICKHOUSE_DSN ───┼───────▶│    DSN, StorePath,   │
 │                              │        │    Prefix })         │
 │ mux.Handle("/explore/",      │        │                      │
 │   RequireSession(            │◀───────┼── .Handler()         │
 │     StripPrefix("/explore",  │        │   (API + SPA)        │
 │       svc.Handler())))       │        └──────────────────────┘
 └──────────────────────────────┘
```

A host application imports the packages, supplies a **tenant-scoped ClickHouse
DSN**, and mounts `explore` behind **its own** authentication. otelhouseview
never authenticates and never enforces tenancy.

**Why this shape and not a service per tenant:** the host already has auth, an
embedded SPA, an ingress, a TLS cert and the per-tenant DSN. A second deployment
per tenant means building each of those a second time, and the second copy is
the one that eventually diverges. Mounting a Go package inside the host that
already holds all of it is strictly less machinery (agentloop#780).

**`explore` is unauthenticated by construction** and MUST be wrapped. It serves
arbitrary SQL; naked on a public listener it is a SQL console for the internet.
This is stated loudly in its package doc, and there is no plan to add auth: auth
inside the package would be a *second* session system per host, competing with
the one the host already has.

**The `/explore` prefix problem, and how it is solved.** An embeddable handler
cannot know its own public URL at build time, so nothing in the bundle may be
root-absolute. The Vite build bakes a placeholder (`/__EXPLORE_BASE__/`) into
every absolute URL it emits — asset `<script>`/`<link>` hrefs *and* an inline
bootstrap that sets `window.__EXPLORE_BASE__` — and the Go handler substitutes
the real mount base into `index.html` when it serves it. `explore/web/src/lib/api.ts`
builds every request URL from that base and falls back to `/` when the token is
unsubstituted (i.e. under `vite dev`). One committed build works at any mount
point with no build-time configuration. The router is hash-based, so client-side
routing survives a prefix untouched.

The host is expected to strip the prefix (`http.StripPrefix`); `Config.Prefix`
exists *only* to generate those absolute URLs. The handler additionally tolerates
an un-stripped mount, so either wiring works — what must be right is
`Config.Prefix` matching the real mount point.

**The SPA build is committed** (`explore/web/build/`). `go:embed` ships whatever
is in the module, and importers run `go get`, not `pnpm`.

## Core substrate

`SQL or saved-query (+params) → result grid → auto time-series chart`.

This single path serves metrics charts, log-volume-over-time, and span rate/latency
without per-signal UIs, and is the exact surface the future AI writes into.
Traces (waterfall) is the one signal needing a bespoke view — it is the single
carve-out, and it **shipped** as the `ciview` package (see
[Public packages](#public-packages-otelstore--ciview) and Out of scope below).

## Auto-chart heuristic

If column 0 is `Date`/`DateTime`/`DateTime64` and ≥1 remaining column is numeric →
time-series (line/area); an optional low-cardinality string column splits series.
Otherwise → table. The user can override the visualization per query.

## Data model (SQLite)

```
saved_query(id, name, description, sql_template, params_json, default_viz,
            created_by, created_at, updated_at)
```

`params_json`: `[{name, type (ClickHouse type), label, widget, default}]`

## Public packages: `otelstore/` + `ciview/` + `explore/`

Three packages are **exported API with external consumers**:

- **`otelstore/`** — read-only, typed ClickHouse client for the stock
  clickhouseexporter schema (`Store`, `GetTrace`, `ListTraces`,
  `ListTracesFiltered`, plus `MemoryStore` as the in-process fake).
- **`ciview/`** — renders a trace as a self-contained HTML page
  (`RenderTrace`, `RenderIndex`).
- **`explore/`** — the SQL workbench as an embeddable sub-application. Public
  surface is deliberately four names: `New(ctx, Config) (*Service, error)`,
  `Config{DSN, StorePath, Prefix}`, `(*Service).Handler() http.Handler`,
  `(*Service).Close() error`. Everything else (the ClickHouse client, the SQLite
  store, the starter queries, the HTTP routing) lives under `explore/internal/`
  and is free to churn.

`explore` **owns its own rendering** — it embeds its own built Svelte bundle and
serves API + SPA from one handler, the same pattern `ciview` uses. Svelte
components are deliberately *not* shared across repos: there is no npm
publishing here, and vendoring rots.

Importers today: **agentloop** (all three) and **otelhouse**'s e2e harness
(`otelstore`). Both pin untagged pseudo-versions, so they pick up `main` on
`go get -u`.

**Consequence, and the rule for anyone working here:** changing the signatures
of `Store` / `GetTrace` / `ListTraces` / `RenderTrace` / `RenderIndex` /
`explore.New` / `Config` / `Handler` / `Close`, or the shape of the types they
carry (`Span`, `LogRecord`, `Trace`, `TraceSummary`), breaks downstream repos.
Prefer additive change; if a break is unavoidable, it is a cross-repo change,
not a local one. Everything under `internal/` is private and free to churn —
put anything that is not a deliberate contract there.

`otelstore` also exposes `WithTracesTable` / `WithLogsTable` and a `DB()`
escape hatch (the raw `*sql.DB`, owned by the store). `DB()` exists because
`GetTrace` is **span-anchored** — it returns `ErrNotFound` when the spans query
is empty even if the trace id has logs — so a consumer that needs to assert
"logs landed" must query around it. otelhouse's e2e does exactly that.

## Tenancy

The library is deliberately **tenant-blind**, and the isolation boundary lives
in ClickHouse, not in Go:

- The [otelhouse](https://github.com/guettli/otelhouse) gateway authenticates a
  per-tenant JWT and stamps `ResourceAttributes['tenant']` on every record at
  write time. Tenancy is established there, fail-closed.
- Reads are constrained by a ClickHouse **row policy** bound to a per-tenant
  read-only user (`<tenant>_ro`). The DSN's identity *is* the tenant.
- Therefore no query in `otelstore` adds a tenant predicate, and none should: a
  filter in Go would look like a security boundary without being one.

The same reasoning is what makes `explore` safe: because the DSN is the
boundary, a tenant can be handed a raw SQL prompt. The worst query they can
write still cannot cross a row policy and still cannot outrun the settings
profile. Add a tenant filter in Go and you have added a *second*, weaker,
drift-prone boundary that hides the real one — so do not.

A single host process is pointed at one DSN and so serves one tenant.
Multi-tenant means "many tenants share one ClickHouse behind row policies", not
"one process fans out across tenants" — there is no tenant switcher in the UI
and no plan for one. A host serving many tenants runs one `explore.Service` per
tenant DSN (each with its own `StorePath`), it does not multiplex one.

## ClickHouse access

- One **read-only** CH user per deployment (creds from a k8s secret / env);
  that user is the tenant boundary (see [Tenancy](#tenancy)).
- Limits enforced on the CH user **profile** (server-side), not app code:
  `max_execution_time`, `max_rows_to_read`, `max_bytes_to_read`,
  `max_result_rows`, `max_memory_usage`.
- Params bound via **native ClickHouse query parameters** — no interpolation.
- Time-series starter queries use `toStartOfInterval(...) ... WITH FILL` for
  gap-free charts.

## Stack

Go + `clickhouse-go/v2`; embedded Svelte 5 SPA (CodeMirror 6 SQL editor,
ECharts); SQLite for saved queries. The SPA lives at `explore/web/` and is
`go:embed`ed by the `explore` package, so a host binary that imports `explore`
is still a single self-contained binary (like agentloop).

## Out of scope for v1

- **AI authoring assist** (phase 3): an agent limited to a read-only ClickHouse
  MCP tool, no shell/fs, no `--approve-all`. Rationale: OTel rows are
  attacker-influenced content, so least-privilege bounds prompt-injection blast
  radius to "ran a SELECT".
- ~~**Trace waterfall** (phase 2).~~ Shipped as the `ciview` package — it
  already existed inside agentloop and was moved here rather than rewritten.
- **Multi-chart dashboards** (phase 2).

## Deployment shape

There is **no standalone otelhouseview service per tenant.** `cmd/otelhouseview`
mounts `explore` at `/` from `CLICKHOUSE_DSN` / `SQLITE_PATH` / `PORT` and is
kept for local development; it authenticates nobody and does not belong on a
public listener. The public static-report host
(`otelhouseview.thomas-guettler.de`) is **gone** — the report pipeline
below survives as CI e2e coverage, not as a product surface.

## Static report pipeline (added)

The query workbench above is the v1 product. A second, self-contained path
produces a **static HTML report** for CI/e2e visibility and is the surface this
repo's own CI exercises end to end.

```
emit (OTLP) → OTel Collector → ClickHouse → genreport → report.json
            → Svelte build (single-file) → dist/index.html → CI artifact
```

- **`ui/`** — Vite + Svelte 5 app built with `vite-plugin-singlefile`. It
  `import`s `src/lib/report-data.json` (baked in, not fetched) so the build is
  one offline HTML file. `src/lib/report.ts` is the schema contract with the Go
  builder; keep the two in sync.
- **`ci/`** — the Dagger pipeline (Go), the single source of truth for tests.
  `cmd/emit` spreads records over a time window and varies severity / metric
  value / span duration so the report has a real time-series, a severity
  breakdown, a metric curve and an OK/ERROR trace mix. `cmd/genreport` queries
  the clickhouseexporter tables (`otel_logs`, `otel_traces`,
  `otel_metrics_gauge`) and fails loudly rather than emit a partial report.
- **Output** — the rendered page is attached to the CI run as an artifact.
  It is deliberately **not deployed**. It used to be uploaded into a ConfigMap
  and served from a public host, which was retired: the report renders
  *synthetic* telemetry that `emit` generated seconds earlier, so it never
  showed a real span, and otelhouse's e2e now asserts the same pipeline **in
  code** — a stronger check than a human squinting at a page. Building it still
  earns its keep as an end-to-end exercise of collector → ClickHouse → render;
  publishing it did not.

Why `ui/` and `explore/web/` are two apps: both are Svelte 5, so the split is
not a framework boundary — it is a delivery boundary. `explore/web/` is a live
SPA that fetches from the Go handler that embeds it; `ui/` bakes its data in at
build time and must render as one offline HTML file with no backend, which rules
out the shared API client and the router. What they *could* share is the chart
layer (`explore/web/src/lib/chartOption.ts` against `ui/src/lib/Sparkline.svelte` and
`StackedTimeSeries.svelte`); that is worth extracting once the report's chart
needs stop moving.
