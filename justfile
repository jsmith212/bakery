default: help

# The bitbake tag the conformance suite pins. Keep in step with the CI cache key.
bb_tag := "2.8.0"

# The websockets version the hashserv gate pins, and it is a MATCHED PAIR with bb_tag.
# bitbake 2.8 calls the LEGACY top-level `websockets.connect(uri, ping_interval=None)`
# (lib/bb/asyncrpc/client.py). That API was deprecated and then REMOVED in websockets 14,
# so an unpinned `pip install websockets` gives a red gate that is not a Bakery bug. 12.0
# is the version contemporary with scarthgap. Bump it only with bb_tag, and only after
# checking that bitbake still calls the legacy API.
websockets_pin := "12.0"

# Install the pre-commit hooks
bootstrap:
  pre-commit install --hook-type commit-msg --hook-type pre-push

# Install the frontend dependencies (skipped when node_modules is already present)
web-deps:
  [ -d web/node_modules ] || (cd web && npm ci)

# Build the SvelteKit app into the directory the Go binary embeds
web: web-deps
  cd web && npm run build

# Run the frontend unit tests
web-test: web-deps
  cd web && npm test

# Type-check the frontend
web-check: web-deps
  cd web && npm run check

# Generate the sqlc repository from internal/db/migrations + internal/db/query
generate:
  go tool sqlc -f internal/db/sqlc.yaml generate

# Fail if the committed queries and the generated repository have drifted
generate-check:
  go tool sqlc -f internal/db/sqlc.yaml diff

# Create a new migration pair (just add-migration add_widgets)
add-migration migration:
  #!/usr/bin/env bash
  # The name is not cosmetic. sqlc reads the migrations dir as its schema and
  # skips rollbacks BY FILENAME SUFFIX, so a down file that is not named
  # *.down.sql gets parsed as schema and its DROP TABLEs corrupt sqlc's catalog.
  # And sqlc applies up-files in LEXICAL order, so the sequence must be
  # zero-padded or 10_ sorts before 9_.
  set -euo pipefail
  seq=$(printf '%06d' $(( $(ls internal/db/migrations/*.up.sql | wc -l) + 1 )))
  touch "internal/db/migrations/${seq}_{{migration}}.up.sql"
  touch "internal/db/migrations/${seq}_{{migration}}.down.sql"
  echo "created internal/db/migrations/${seq}_{{migration}}.{up,down}.sql"

# Build the server
build: web generate
  CGO_ENABLED=0 go build -o ./build/bakery .

# Build the whole release artifact matrix into dist/ exactly as the release job
# does -- five platforms, archives, SHA256SUMS -- publishing nothing. Needs
# goreleaser on PATH (https://goreleaser.com/install/; OSS, not Pro). Version
# selection, notes, and publishing live in .github/workflows/release.yml.
release-snapshot:
  goreleaser release --snapshot --clean

# Run the server (needs Postgres: `just db-up` then export DB_URL -- see the README)
run: web generate
  # `serve` is not optional: the binary is also the API client, so `go run .` with
  # no verb is a Kong usage error, not a server. serve reads DB_URL (and the rest)
  # from the environment; it does not load stack.env, which is the compose stack's
  # file and whose DB host is `db`, not localhost.
  go run . serve

# Run the Vite dev server against a locally running `just run`
#
# The browser only ever talks to :5173; vite.config.ts proxies /api/v1 and
# /cache through to 127.0.0.1:8080, so the SameSite=Lax session cookie stays
# genuinely same-origin. There is no CORS anywhere in this product and none is
# needed -- production is one binary, one port, one origin, and this recipe
# keeps development the same shape.
dev: web-deps
  cd web && npm run dev

# Run unit tests (Go + frontend). DB tests spawn an ephemeral Postgres via docker,
# or use TEST_DB_URL if it is exported.
test: web generate web-test
  go test -v ./...

# Run every Go test and FAIL if any of them SKIPPED (a skipped suite is not a passing suite)
test-db: generate
  # dbtest SKIPS -- it does not fail -- when it can find neither docker nor
  # TEST_DB_URL. That is right on a laptop and catastrophic in CI: the entire
  # database half of the suite would go green without executing a single query,
  # and nothing in the log would say so. So CI runs THIS recipe, where a skip is
  # a failure.
  #
  # bash, and pipefail, on purpose: with `sh` the exit status of `go test | tee`
  # is tee's, so a failing test would be reported as a pass.
  mkdir -p build
  bash -euo pipefail -c 'go test -v -count=1 ./internal/... 2>&1 | tee build/test-db.log'
  ! grep -q -- '--- SKIP' build/test-db.log || { grep -- '--- SKIP' build/test-db.log; echo 'FAIL: tests were SKIPPED. They did not run, so they did not pass. Start docker, or export TEST_DB_URL.'; exit 1; }
  @echo "no skipped tests"

# Drive the REAL bitbake fetcher + real wget against the real sstate backend (a skip FAILS)
conformance: generate
  # Not in `just test-db` (which globs ./internal/...): this suite legitimately skips
  # on a laptop with no bitbake checkout. This recipe is its home, and it provides the
  # checkout so a skip here means the real client did not run -- which is a failure.
  # bash + pipefail (not a shebang recipe, and not `sh`): with `sh` the exit status
  # of `go test | tee` is tee's, so a failing suite would report as a pass -- exactly
  # the trap `test-db` documents. Use an existing BB_LIB if the caller set one (CI
  # restores a cached clone into it); otherwise clone the pinned tag, shallow.
  mkdir -p build
  bash -euo pipefail -c ' \
    if [ -z "${BB_LIB:-}" ]; then \
      if [ ! -d build/bitbake/lib ]; then \
        echo "cloning bitbake {{bb_tag}} (shallow) into build/bitbake ..."; \
        git clone --depth 1 --branch {{bb_tag}} https://git.openembedded.org/bitbake build/bitbake; \
      fi; \
      BB_LIB="$(pwd)/build/bitbake/lib"; \
    fi; \
    export BB_LIB; \
    test -d "${BB_LIB}/bb/fetch2" || { echo "FAIL: no bb/fetch2 under BB_LIB=${BB_LIB}"; exit 1; }; \
    go test -v -count=1 ./test/conformance/... 2>&1 | tee build/conformance.log; \
    if grep -q -- "--- SKIP" build/conformance.log; then \
      grep -- "--- SKIP" build/conformance.log; \
      echo "FAIL: the conformance suite SKIPPED -- the real client did not run. Ensure docker or TEST_DB_URL, python3, wget, and a bitbake checkout (BB_LIB)."; \
      exit 1; \
    fi; \
    echo "conformance: the real bitbake fetcher ran green" '

# Drive bitbake's OWN hashserv suite + the real bitbake-hashclient at the hashserv backend (a skip FAILS)
hashserv-conformance: generate
  # M3's gate, and it has TWO halves. Half 1 is bitbake's own hashserv test suite in
  # external mode (BB_TEST_HASHSERV), authenticated with a real write-scoped bkry_ key --
  # its setUp AND tearDown both call remove(), which Bakery maps to a write key, so an
  # unauthenticated run errors in every setUp. Half 2 is the real bitbake-hashclient
  # binary, which upstream's external suite NEVER runs (all its run_hashclient call sites
  # spawn a local unix:// python server instead).
  #
  # Not in `just test-db` (which globs ./internal/...): this suite legitimately skips on a
  # laptop with no bitbake checkout. This recipe is its home, and it provides the checkout
  # and the pinned websockets -- so a skip here means the real client did not run, which is
  # a failure.
  #
  # bash + pipefail (not `sh`): with `sh` the exit status of `go test | tee` is TEE's, so a
  # failing suite would report as a pass -- the same trap `test-db` and `conformance`
  # document.
  #
  # websockets goes in with `pip install --target`, not into the interpreter: ubuntu-latest
  # (and Debian) mark the system python EXTERNALLY-MANAGED (PEP 668), where a bare
  # `pip3 install websockets` dies with `error: externally-managed-environment`. --target
  # writes a plain directory, which PYTHONPATH picks up and pip does not refuse -- and it
  # keeps the pin off the interpreter the rest of the machine shares.
  mkdir -p build
  bash -euo pipefail -c ' \
    if [ -z "${BB_LIB:-}" ]; then \
      if [ ! -d build/bitbake/lib ]; then \
        echo "cloning bitbake {{bb_tag}} (shallow) into build/bitbake ..."; \
        git clone --depth 1 --branch {{bb_tag}} https://git.openembedded.org/bitbake build/bitbake; \
      fi; \
      BB_LIB="$(pwd)/build/bitbake/lib"; \
    fi; \
    export BB_LIB; \
    test -d "${BB_LIB}/hashserv" || { echo "FAIL: no hashserv/ under BB_LIB=${BB_LIB}"; exit 1; }; \
    test -x "$(dirname "${BB_LIB}")/bin/bitbake-hashclient" || { echo "FAIL: no bitbake-hashclient beside BB_LIB=${BB_LIB}"; exit 1; }; \
    pylib="$(pwd)/build/hashserv-pylib"; \
    if [ ! -d "${pylib}/websockets-{{websockets_pin}}.dist-info" ]; then \
      echo "installing websockets=={{websockets_pin}} into ${pylib} (bitbake {{bb_tag}} needs the legacy connect API) ..."; \
      rm -rf "${pylib}"; \
      python3 -m pip install --quiet --disable-pip-version-check --target "${pylib}" "websockets=={{websockets_pin}}"; \
    fi; \
    export PYTHONPATH="${pylib}"; \
    go test -v -count=1 -timeout 20m ./test/hashserv/... 2>&1 | tee build/hashserv-conformance.log; \
    if grep -q -- "--- SKIP" build/hashserv-conformance.log; then \
      grep -- "--- SKIP" build/hashserv-conformance.log; \
      echo "FAIL: the hashserv conformance suite SKIPPED -- the real client did not run. Ensure docker or TEST_DB_URL, python3 with pip, and a bitbake checkout (BB_LIB)."; \
      exit 1; \
    fi; \
    echo "hashserv-conformance: bitbake own suite + the real bitbake-hashclient ran green" '

# Drive the REAL Bazel REAPI (remote-apis-sdks) + real ccache + real sccache at the M4 backends (a skip FAILS)
bazel-conformance: generate
  # M4's gate, and it has THREE clients. Client 1 is the remote-apis-sdks Go client (a
  # test-only dep) driving all eight cache RPCs over a real gRPC listener -- it needs no
  # external binary, so it runs everywhere dbtest can reach a database. Clients 2 and 3 are
  # the REAL ccache and sccache binaries against the /ac and sccache WebDAV mounts: the
  # recorder proves ccache hit /ac and NEVER /cas, and that sccache did PROPFIND-then-PUT
  # (the silently-read-only trap opendal falls into when the collection probe fails).
  #
  # Not in `just test-db` (which globs ./internal/...): the ccache/sccache halves legitimately
  # skip on a laptop with no client installed. This recipe is their home, and CI installs
  # ccache + sccache -- so a skip here means a real client did not run, which is a failure.
  #
  # bash + pipefail (not `sh`): with `sh` the exit status of `go test | tee` is TEE's, so a
  # failing suite would report as a pass -- the same trap `test-db`, `conformance` and
  # `hashserv-conformance` document.
  mkdir -p build
  bash -euo pipefail -c ' \
    go test -v -count=1 -timeout 20m ./test/bazel/... 2>&1 | tee build/bazel-conformance.log; \
    if grep -q -- "--- SKIP" build/bazel-conformance.log; then \
      grep -- "--- SKIP" build/bazel-conformance.log; \
      echo "FAIL: the bazel conformance suite SKIPPED -- a real client did not run. Ensure docker or TEST_DB_URL, ccache, sccache and a C compiler (cc)."; \
      exit 1; \
    fi; \
    echo "bazel-conformance: remote-apis-sdks + the real ccache and sccache ran green" '

# Drive the REAL containerd resolver + crane + skopeo at the M5 OCI mirror (a skip FAILS)
oci-conformance: generate
  # M5's gate, and it has FOUR clients against a FAKE UPSTREAM registry on loopback -- so
  # it needs no network and no daemon. Client 1 is containerd's own resolver (a test-only
  # dep): it is the only one that exercises ?ns=, the HEAD digest fast path and the OAuth2
  # POST token endpoint an identitytoken credential forces. Client 2 is
  # go-containerregistry (crane's engine), pulling by tag AND by digest through BOTH route
  # families and validating a whole image, layers included. Client 3 is the REAL skopeo
  # binary -- containers/image is the only stack that hard-requires the bare-root GET /v2/
  # ping and harvests its auth challenges from that response alone. Client 4 is Docker
  # Engine BY SHAPE: the daemon cannot run in CI, so moby's own URL construction
  # (ValidateMirror's trailing slash, then ResolveReference) is replicated and driven at
  # Bakery directly -- drop that slash and every pull silently goes to Docker Hub instead.
  #
  # EVERY SUCCESS ASSERTION IS PAIRED WITH AN UPSTREAM REQUEST COUNT. All four clients
  # fall back to the real registry on ANY mirror failure, so "the pull worked" proves
  # nothing on its own; "the pull worked and the upstream saw zero requests" proves Bakery
  # served it.
  #
  # Not in `just test-db` (which globs ./internal/...): the skopeo half legitimately skips
  # on a laptop with no client installed. This recipe is its home, and CI installs skopeo
  # -- so a skip here means a real client did not run, which is a failure.
  #
  # bash + pipefail (not `sh`): with `sh` the exit status of `go test | tee` is TEE's, so a
  # failing suite would report as a pass -- the same trap `test-db`, `conformance`,
  # `hashserv-conformance` and `bazel-conformance` document.
  mkdir -p build
  bash -euo pipefail -c ' \
    go test -v -count=1 -timeout 20m ./test/oci/... 2>&1 | tee build/oci-conformance.log; \
    if grep -q -- "--- SKIP" build/oci-conformance.log; then \
      grep -- "--- SKIP" build/oci-conformance.log; \
      echo "FAIL: the oci conformance suite SKIPPED -- a real client did not run. Ensure docker or TEST_DB_URL and skopeo."; \
      exit 1; \
    fi; \
    echo "oci-conformance: containerd + crane + skopeo + the Docker Engine URL shape ran green" '

# Run the shared storage conformance suite against BOTH drivers, S3 on a real minio (a skip FAILS)
storage-conformance: generate
  # Feedback wave 1's gate for the S3 driver. The suite in
  # internal/storage/conformance_test.go is Store-interface-only and runs twice:
  # against Local, and against a REAL minio the harness starts with `docker run`
  # (path-style addressing, the mechanism DESIGN.md prescribes for
  # minio/Ceph/Garage/R2).
  #
  # A FAKE S3 WOULD PASS EVERY ONE OF THESE TESTS AND PROVE NOTHING. What the
  # driver has to get right IS S3's semantics -- read-after-write consistency, a
  # DELETE of an absent key succeeding, a server-side copy, HeadObject's bodiless
  # 404 -- and a fake agrees with whatever the driver happens to do.
  #
  # So a SKIP here is a failure, not a pass: it means the S3 half never ran a
  # single assertion against real S3 semantics, which is exactly the trap every
  # other conformance recipe documents. The suite skips (rather than fails) when
  # it can find neither docker nor TEST_S3_ENDPOINT, which is right on a laptop
  # and catastrophic in CI -- this recipe is where that distinction is made.
  #
  # bash + pipefail (not `sh`): with `sh` the exit status of `go test | tee` is
  # TEE's, so a failing suite would report as a pass -- the same trap `test-db`,
  # `conformance`, `hashserv-conformance`, `bazel-conformance` and
  # `oci-conformance` document.
  mkdir -p build
  bash -euo pipefail -c ' \
    go test -v -count=1 -race -timeout 15m ./internal/storage/... 2>&1 | tee build/storage-conformance.log; \
    if grep -q -- "--- SKIP" build/storage-conformance.log; then \
      grep -- "--- SKIP" build/storage-conformance.log; \
      echo "FAIL: the storage conformance suite SKIPPED -- the S3 driver was never exercised against real S3 semantics. Ensure docker, or export TEST_S3_ENDPOINT / TEST_S3_ACCESS_KEY / TEST_S3_SECRET_KEY."; \
      exit 1; \
    fi; \
    echo "storage-conformance: the shared suite ran green against local AND a real minio" '

# Drive the REAL bakery binary + a real Chromium through the console (a skip FAILS)
web-e2e: build
  # `build` gives this a fresh ./build/bakery, and playwright.config.ts's
  # webServer spawns exactly that binary with `cwd` pinned at the repo root --
  # NOT `web/e2e/` (Playwright's own default), which is the trap: a bare
  # relative `build/bakery` resolved against the config file's directory is
  # `web/e2e/build/bakery`, a path that does not exist.
  #
  # Chromium only, installed here rather than assumed present -- `npx
  # playwright install` is idempotent and near-instant on a cache hit, same
  # shape as `hashserv-conformance` installing its pinned websockets into a
  # cached target dir on every run.
  #
  # PLAYWRIGHT_JSON_OUTPUT_NAME + `jq -e '.stats.skipped == 0'`, never a
  # stdout grep: Playwright's own JSON reporter writes structured output
  # there, and `--reporter=list,json` on the command line keeps the `list`
  # progress output human-readable in the log without interleaving into the
  # JSON file. A `.skip()`/`.fixme()` left in a spec, or `forbidOnly` firing
  # on a stray `.only`, must fail this exactly like a `--- SKIP` fails the Go
  # conformance recipes -- a suite that quietly ran fewer tests than it
  # claims is not a passing suite.
  # bash + pipefail (not `sh`): with `sh` the exit status of `playwright test |
  # tee` is tee's, so a failing suite would report as a pass -- the same trap
  # `test-db` and every conformance recipe document.
  #
  # PLAYWRIGHT_JSON_OUTPUT_NAME resolves relative to the CONFIG FILE's
  # directory (web/e2e/), not the process cwd -- ../../build lands at the
  # repo root's build/, matching every other recipe's log location.
  mkdir -p build
  cd web && npx playwright install chromium
  bash -euo pipefail -c ' \
    cd web && \
    PLAYWRIGHT_JSON_OUTPUT_NAME=../../build/web-e2e.json npx playwright test -c e2e/playwright.config.ts --reporter=list,json 2>&1 | tee ../build/web-e2e.log; \
    cd .. && \
    jq -e ".stats.skipped == 0" build/web-e2e.json > /dev/null || { \
      echo "FAIL: the web e2e suite SKIPPED a test -- it did not run, so it did not pass."; \
      exit 1; \
    }; \
    echo "web-e2e: the real bakery binary + a real Chromium ran the console flow green" '

# Run the race detector
race: web generate
  go test -race ./...

# Run tests with coverage (frontend unit tests run first so CI gates on them)
coverage: web generate web-test
  mkdir -p build
  go test -v -coverprofile=build/coverage.out ./...
  go tool cover -func=build/coverage.out
  go tool cover -html=build/coverage.out -o build/coverage.html

# Build and run the WHOLE thing locally with one command: Postgres (started if
# absent), migrations at boot, dev-login enabled, console at http://127.0.0.1:8080.
#
# Dev-login seeds dev@bakery.local as a site admin with dev-org/playground, so the
# first click on "Sign in without auth" lands in a working console. This reuses the
# bakery-testdb container, so state persists across restarts (just db-down resets).
# NEVER a production shape: DEV_LOGIN_ENABLED also drops Secure off the session
# cookie, which is exactly what makes plain-http localhost work.
demo: build
  @docker inspect bakery-testdb > /dev/null 2>&1 || just db-up
  @until docker exec bakery-testdb pg_isready -U postgres -q; do sleep 0.3; done
  DB_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable' \
    DEV_LOGIN_ENABLED=1 ./build/bakery serve

# Start a shared Postgres for the local test loop (faster than a container per package)
db-up:
  docker run -d --name bakery-testdb -p 127.0.0.1:5432:5432 \
    -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=postgres \
    postgres:18-alpine
  @echo "export TEST_DB_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable"

# Stop the shared test Postgres
db-down:
  -docker rm -f bakery-testdb

# Validate the shipped k8s manifests against the Kubernetes API schema, plus
# the three policy properties that a schema-valid manifest can still violate:
# strategy: Recreate (feedback wave 1, spec section 7 -- see docs/deploy/k8s.md
# for why RollingUpdate permanently wedges at replicas=1), a NetworkPolicy
# actually shipping in the set (F8: a Service is discovery, not a firewall --
# /metrics needs 0.0.0.0 to be reachable by its own Service, so the
# NetworkPolicy is what keeps it off-limits to the rest of the cluster), and an
# ipBlock allowance for the probe port (the kubelet is not in a namespace, so a
# policy whose only allows are namespaceSelectors drops every health probe on
# Calico/Weave/Antrea and CrashLoopBackOffs the pod).
#
# kubeconform via `go tool` (go.mod's tool directive), not a downloaded
# platform release binary: it resolves through the same module-proxy cache
# every other Go dependency does, and it needs no per-platform asset selection
# logic.
#
# THE SCHEMAS ARE VENDORED (docs/deploy/schemas/) AND -schema-location POINTS AT
# THEM. kubeconform's default schema location is raw.githubusercontent.com,
# fetched per resource per run: this recipe is in `check`, the fastest and most
# frequently run one and the CI job that gates everything else, so leaving it on
# the network means `just check` hard-fails offline and a GitHub blip reds a
# gate for a lint failure nobody wrote. See docs/deploy/schemas/README.md.
#
# THE SUMMARY GREP ASSERTS A POSITIVE COUNT AND A SKIP BUDGET, not merely the
# absence of failures. With -ignore-missing-schemas a schema the catalog cannot
# serve is Skipped, not Errored -- so `Valid: 0, Invalid: 0, Errors: 0,
# Skipped: 8` matches "Invalid: 0, Errors: 0", exits 0, and reports success
# having validated NOTHING. That is one -kubernetes-version bump or one schema
# path change away at all times. The single tolerated skip is
# servicemonitor.yaml, a Prometheus Operator CRD whose own controller validates
# it at apply time; a second skip is a failure.
k8s-check:
  mkdir -p build
  bash -euo pipefail -c ' \
    go tool kubeconform -kubernetes-version 1.31.0 -strict -ignore-missing-schemas -summary \
      -schema-location "docs/deploy/schemas/{{{{ .NormalizedKubernetesVersion }}-standalone{{{{ .StrictSuffix }}/{{{{ .ResourceKind }}{{{{ .KindSuffix }}.json" \
      docs/deploy/k8s/*.yaml 2>&1 | tee build/k8s-check.log; \
    grep -qE "Valid: [1-9][0-9]*, Invalid: 0, Errors: 0, Skipped: [01]$" build/k8s-check.log || { \
      echo "FAIL: kubeconform validated nothing, skipped more than servicemonitor.yaml, or " \
           "found an invalid manifest. A skip is NOT a pass: check that " \
           "docs/deploy/schemas/ carries a schema for every kind shipped under " \
           "docs/deploy/k8s/, at the -kubernetes-version this recipe pins"; \
      exit 1; \
    }; \
    grep -A2 "^  strategy:" docs/deploy/k8s/deployment.yaml | grep -q "type: Recreate" || { \
      echo "FAIL: deployment.yaml does not ship strategy: Recreate -- see docs/deploy/k8s.md " \
           "for why the default RollingUpdate permanently wedges this singleton at replicas=1"; \
      exit 1; \
    }; \
    grep -qr "^kind: NetworkPolicy" docs/deploy/k8s/ || { \
      echo "FAIL: no NetworkPolicy manifest shipped -- the metrics Service is discovery, " \
           "not a firewall, and 0.0.0.0:9090 is reachable by any pod in the cluster without one"; \
      exit 1; \
    }; \
    grep -A6 "ipBlock:" docs/deploy/k8s/networkpolicy.yaml | grep -q "port: http" || { \
      echo "FAIL: networkpolicy.yaml does not admit the probe port (http) from an ipBlock. " \
           "Health probes come from the KUBELET on the node, not from a namespace, so a " \
           "namespaceSelector cannot admit them: on Calico/Weave/Antrea every probe is " \
           "dropped, the startupProbe burns its budget, and the pod CrashLoopBackOffs with " \
           "a healthy process. See docs/deploy/k8s.md"; \
      exit 1; \
    }; \
    echo "k8s-check: manifests are schema-valid (offline), Recreate, NetworkPolicy-guarded, " \
         "and probe-reachable" '

# Run all code checks
#
# web-test is in this list for SPEED, not coverage: vitest is already CI-gated
# through `just coverage` in the build job, so this only moves a frontend
# failure out of the slow job and into the fast one, where it costs half a
# second instead of a full `go test -race`.
check: check-format vet lint web-check web-test k8s-check

# Check the format of the code
check-format:
  if [ -n "$(gofmt -l .)" ]; then gofmt -l .; exit 1; fi

# Run the vet tool
vet: web generate
  go vet ./...

# Run the golangci-lint tool
lint: web generate
  go tool golangci-lint run

# Format all code
format:
  go tool golangci-lint fmt

# Clean up all build artifacts
clean:
  go clean
  -rm -rf ./build
  -rm -rf ./tmp
  -rm -rf ./internal/db/repository
  -rm -rf ./web/.svelte-kit
  -rm -rf ./web/node_modules
  -find ./web/dist -mindepth 1 ! -name '.gitkeep' -delete

# Build the server docker image
image:
  docker build -t ghcr.io/jsmith212/bakery:latest .

# Start the application stack
start: image
  docker compose up -d

# Stop the application stack
stop:
  docker compose down

# Print this help message
help:
  just --list
