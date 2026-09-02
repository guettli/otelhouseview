// The Dagger pipeline in ci/ is the single source of truth for otelhouseview CI.
// `make ci` (or `cd ci && go run .`) runs locally exactly what GitHub Actions
// runs, so a green local run implies a green CI run.
//
// What it does, end to end:
//
//  1. Go checks (gofmt, go vet, go build, go test) on the ci module.
//  2. e2e harness (one container): upstream OTel Collector + the in-repo
//     `emit` binary generate spread logs/metrics/traces over OTLP into an
//     ephemeral ClickHouse; `genreport` then reads the tables straight out of
//     ClickHouse and writes report.json.
//  3. Svelte build: a node container bakes report.json into the single-file
//     static HTML report (ui/ → dist/index.html).
//  4. Upload (push to main only): the report is written into the
//     `otelhouseview-report` ConfigMap in the cluster via a scoped kubectl token,
//     where a caddy Deployment serves it.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"dagger.io/dagger"
)

// Pinned upstream OTel Collector contrib image. Drives the clickhouseexporter
// schema the UI targets; bump when the schema changes.
const otelCollectorVersion = "0.114.0"

// ClickHouse credentials for the ephemeral CI server. Centralised so the YAML
// stays generic and consumes them via ${env:...}.
const (
	clickhouseUser     = "test"
	clickhousePassword = "test"
	clickhouseDB       = "test"
)

// reportOut is where the rendered report lands on the host; the workflow
// attaches it to the run as an artifact.
const reportOut = "report/index.html"

func main() {
	ctx := context.Background()
	if err := pipeline(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func pipeline(ctx context.Context) error {
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return fmt.Errorf("dagger connect: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Mount the repo. Exclude heavy/irrelevant trees so the build context stays
	// small and node_modules from a local run never leaks into the container.
	src := client.Host().Directory("..", dagger.HostDirectoryOpts{
		Exclude: []string{".git/", "ui/node_modules/", "ui/dist/", "explore/web/node_modules/"},
	})

	goMod := client.CacheVolume("otelhouseview-go-mod")
	goBuild := client.CacheVolume("otelhouseview-go-build")

	clickhouse := client.Container().
		From("clickhouse/clickhouse-server:25.5").
		WithEnvVariable("CLICKHOUSE_USER", clickhouseUser).
		WithEnvVariable("CLICKHOUSE_PASSWORD", clickhousePassword).
		WithEnvVariable("CLICKHOUSE_DB", clickhouseDB).
		WithExposedPort(9000).
		WithExposedPort(8123).
		AsService()

	clickhouseDSN := fmt.Sprintf("clickhouse://%s:%s@clickhouse:9000/%s",
		clickhouseUser, clickhousePassword, clickhouseDB)

	goBase := client.Container().
		From("golang:1.26-alpine").
		WithMountedCache("/go/pkg/mod", goMod).
		WithMountedCache("/root/.cache/go-build", goBuild).
		WithMountedDirectory("/src", src).
		WithWorkdir("/src/ci")

	// gofmt over the whole tree.
	if _, err = goBase.WithExec([]string{"sh", "-c",
		`out=$(gofmt -l /src); if [ -n "$out" ]; then echo "unformatted: $out" >&2; exit 1; fi`,
	}).Sync(ctx); err != nil {
		return fmt.Errorf("gofmt: %w", err)
	}
	// go vet + build + unit tests for the ci module.
	for _, step := range [][]string{
		{"go", "vet", "./..."},
		{"go", "build", "./..."},
		{"go", "test", "-count=1", "./..."},
	} {
		if _, err = goBase.WithExec(step).Sync(ctx); err != nil {
			return fmt.Errorf("ci: %v: %w", step, err)
		}
	}
	// Same checks for the otelhouseview service module at the repo root.
	// CGO is off so paulmach/orb (an indirect clickhouse-go dep) picks its
	// pure-Go path — the alpine image has no C toolchain.
	appBase := goBase.WithWorkdir("/src").WithEnvVariable("CGO_ENABLED", "0")
	for _, step := range [][]string{
		{"go", "vet", "./..."},
		{"go", "build", "./..."},
		{"go", "test", "-count=1", "./..."},
	} {
		if _, err = appBase.WithExec(step).Sync(ctx); err != nil {
			return fmt.Errorf("app: %v: %w", step, err)
		}
	}
	// SPA unit tests (autoViz heuristic).
	if _, err = runWebTests(ctx, client, src); err != nil {
		return fmt.Errorf("web tests: %w", err)
	}

	// e2e: emit telemetry → collector → ClickHouse → genreport → report.json.
	reportJSON, err := runE2E(ctx, client, clickhouse, clickhouseDSN, src, goBase)
	if err != nil {
		return fmt.Errorf("e2e: %w", err)
	}

	// Bake report.json into the single-file Svelte report.
	indexHTML := buildReport(client, src, reportJSON)
	if _, err = indexHTML.Sync(ctx); err != nil {
		return fmt.Errorf("svelte build: %w", err)
	}

	// Export the report to the host so the workflow can attach it to the run as
	// an artifact. It used to be uploaded into a ConfigMap and served from a
	// public host; that host is gone. The report renders synthetic telemetry
	// that `emit` generated seconds earlier, so it never showed a real span —
	// and otelhouse's e2e now asserts the same pipeline in code, which is a
	// stronger check than a human squinting at a page. Building it still earns
	// its keep as an end-to-end exercise of collector -> ClickHouse -> render;
	// publishing it did not.
	if _, err = indexHTML.Export(ctx, reportOut); err != nil {
		return fmt.Errorf("export report: %w", err)
	}
	fmt.Fprintf(os.Stderr, "report written to %s\n", reportOut)

	fmt.Println("All checks passed.")
	return nil
}

// runE2E runs the whole telemetry harness inside ONE container (upstream
// collector + emit + genreport as local processes) against a single inbound
// ClickHouse service binding, then exports the report.json genreport wrote.
//
// One container, not chained Dagger services: a container that itself has a
// WithServiceBinding, run as a Dagger service, has been observed to hang the
// step on Dagger v0.21.x even with skip-healthcheck (see otelhouse #50). Running
// the collector as a background process inside one container avoids that.
func runE2E(
	ctx context.Context,
	client *dagger.Client,
	clickhouse *dagger.Service,
	clickhouseDSN string,
	src *dagger.Directory,
	goBase *dagger.Container,
) (*dagger.File, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	collectorImage := fmt.Sprintf("otel/opentelemetry-collector-contrib:%s", otelCollectorVersion)
	collectorBin := client.Container().From(collectorImage).File("/otelcol-contrib")

	harness := goBase.
		WithFile("/usr/local/bin/otelcol-contrib", collectorBin).
		WithFile("/etc/otelcol/config.yaml", src.File("ci/otel-collector-config.yaml")).
		WithServiceBinding("clickhouse", clickhouse).
		WithEnvVariable("CLICKHOUSE_HOST", "clickhouse").
		WithEnvVariable("CLICKHOUSE_DB", clickhouseDB).
		WithEnvVariable("CLICKHOUSE_USER", clickhouseUser).
		WithEnvVariable("CLICKHOUSE_PASSWORD", clickhousePassword).
		WithEnvVariable("CLICKHOUSE_DSN", clickhouseDSN).
		WithEnvVariable("OTELHOUSEVIEW_REPO", os.Getenv("OTELHOUSEVIEW_REPO")).
		WithEnvVariable("OTELHOUSEVIEW_COMMIT", os.Getenv("OTELHOUSEVIEW_COMMIT")).
		WithEnvVariable("OTELHOUSEVIEW_RUN_URL", os.Getenv("OTELHOUSEVIEW_RUN_URL")).
		WithExec([]string{"sh", "-c", e2eScript})

	if _, err := harness.Sync(ctx); err != nil {
		return nil, fmt.Errorf("e2e harness: %w", err)
	}
	return harness.File("/out/report.json"), nil
}

// runWebTests runs `pnpm install` + `pnpm run test` for the workbench SPA under
// explore/web/. The vitest suite covers autoViz.ts (the auto-chart heuristic on
// which the whole "grid or line" UX pivots) and api.ts (the mount-base URL
// building that lets a host app mount explore under any prefix).
func runWebTests(ctx context.Context, client *dagger.Client, src *dagger.Directory) (*dagger.Container, error) {
	pnpmStore := client.CacheVolume("otelhouseview-pnpm-store")
	return client.Container().
		From("node:22-alpine").
		WithMountedCache("/root/.local/share/pnpm/store", pnpmStore).
		WithMountedDirectory("/app", src.Directory("explore/web")).
		WithWorkdir("/app").
		WithExec([]string{"corepack", "enable"}).
		WithExec([]string{"corepack", "prepare", "pnpm@9.15.4", "--activate"}).
		WithExec([]string{"pnpm", "install", "--frozen-lockfile"}).
		WithExec([]string{"pnpm", "run", "test"}).
		Sync(ctx)
}

// buildReport bakes report.json into ui/src/lib/report-data.json and runs the
// Vite/Svelte build, returning the single self-contained dist/index.html.
func buildReport(client *dagger.Client, src *dagger.Directory, reportJSON *dagger.File) *dagger.File {
	pnpmStore := client.CacheVolume("otelhouseview-pnpm-store")
	return client.Container().
		From("node:22-alpine").
		WithMountedCache("/root/.local/share/pnpm/store", pnpmStore).
		WithMountedDirectory("/app", src.Directory("ui")).
		WithFile("/app/src/lib/report-data.json", reportJSON).
		WithWorkdir("/app").
		WithExec([]string{"corepack", "enable"}).
		WithExec([]string{"corepack", "prepare", "pnpm@9.15.4", "--activate"}).
		WithExec([]string{"pnpm", "install", "--frozen-lockfile"}).
		WithExec([]string{"pnpm", "run", "build"}).
		File("/app/dist/index.html")
}

// e2eScript builds emit + genreport, runs the upstream collector as a
// background process, emits spread logs/metrics/traces over OTLP, waits for the
// batch flush, then runs genreport to write /out/report.json.
const e2eScript = `set -eu

echo "[e2e] building emit and genreport"
go build -o /usr/local/bin/emit      ./cmd/emit
go build -o /usr/local/bin/genreport ./cmd/genreport

mkdir -p /tmp/e2e /out

echo "[e2e] starting otel-collector-contrib (background)"
/usr/local/bin/otelcol-contrib --config=/etc/otelcol/config.yaml > /tmp/e2e/collector.log 2>&1 &
COLLECTOR_PID=$!

cleanup() {
  status=$?
  kill "$COLLECTOR_PID" 2>/dev/null || true
  if [ "$status" -ne 0 ]; then echo "=== collector.log ==="; cat /tmp/e2e/collector.log 2>/dev/null || true; fi
}
trap cleanup EXIT

echo "[e2e] waiting for collector to be ready"
for i in $(seq 1 60); do
  grep -q "Everything is ready" /tmp/e2e/collector.log 2>/dev/null && break
  sleep 1
done
grep -q "Everything is ready" /tmp/e2e/collector.log 2>/dev/null || { echo "collector not ready in 60s" >&2; exit 1; }

echo "[e2e] emitting logs/metrics/traces over OTLP"
/usr/local/bin/emit -endpoint 127.0.0.1:4317 -logs 300 -traces 30 -window 15m

echo "[e2e] waiting for collector to flush rows to clickhouse"
sleep 4

echo "[e2e] building report.json from clickhouse"
/usr/local/bin/genreport -assert -dsn "$CLICKHOUSE_DSN" -out /out/report.json
echo "[e2e] report.json ready"
`
