# Contributing to tend

Thanks for your interest in contributing to **tend**, a self-hostable cron /
job-runner with run monitoring, alerting, and dead-man's-switch heartbeats,
shipped as a single static Go binary. Contributions of all sizes are welcome:
bug fixes, features, docs, and tests.

This guide covers everything you need to get a change merged: setting up your
environment, running exactly what CI runs, the project conventions, the pull
request flow, and the one-time **CLA sign-off**.

---

## Prerequisites

- **Go 1.25** (the module targets `go 1.25.0`, see [`go.mod`](go.mod)). The
  recommended way to match CI is to let your tooling read the version from
  `go.mod`.
- **git**.
- **A C compiler (gcc/clang)**: only needed to run the tests, because the Go
  race detector (`-race`) links the C race runtime and therefore requires
  `CGO_ENABLED=1`. The production binary itself is CGO-free (pure-Go SQLite
  driver), so you do **not** need cgo just to build.
- **Docker** (optional): only if you want to run the Postgres test path locally
  (see below).

No cgo toolchain is required for `make build`; it is only required for
`go test -race`.

---

## Clone and build

```sh
git clone https://github.com/marsadhq/tend.git
cd tend
make build
```

`make build` compiles the CLI to `bin/tend` (it runs `go build -o bin/tend
./cmd/tend`). You can run it directly with `make run` (`go run ./cmd/tend`).

---

## Running the tests (mirror CI exactly)

CI is the source of truth. The required-green checks are **gofmt, go vet, go
build, and the full test suite under `-race` on BOTH SQLite and Postgres**. Run
the same things locally before you open a PR.

### Formatting and vet

```sh
make lint        # runs: go vet ./...
gofmt -l ./cmd ./internal   # must print nothing; lists any unformatted files
```

`make lint` currently runs `go vet ./...`. CI additionally fails if any file
under `cmd/` or `internal/` is not gofmt-formatted, so run `gofmt -l ./cmd
./internal` (it should print nothing) and `gofmt -w` to fix anything it lists.

> Note: `make lint` runs `go vet` today; `golangci-lint` may be adopted later.
> Until then, gofmt + vet are the bar.

### Tests: SQLite (default)

```sh
make test                    # go test ./...  (quick local loop)
go test ./... -race -count=1 # what CI runs for the SQLite matrix leg
```

When `TEND_TEST_PG` is **unset**, the store tests run against SQLite. This is
the default path.

### Tests: Postgres

The store has two backends that must stay behaviorally identical (see
"Conventions" below), so CI runs the entire suite a second time against
Postgres. To run that path locally, start a Postgres and point `TEND_TEST_PG`
at it:

```sh
# Start a throwaway Postgres (matches the CI service container):
docker run --rm -d --name tend-pg \
  -e POSTGRES_USER=tend -e POSTGRES_PASSWORD=tend -e POSTGRES_DB=tend \
  -p 5432:5432 postgres:16

# Run the suite against it, exactly as CI does:
TEND_TEST_PG="postgres://tend:tend@localhost:5432/tend?sslmode=disable" \
  go test ./... -race -count=1

docker rm -f tend-pg   # cleanup
```

When `TEND_TEST_PG` is set, the store tests connect to that DSN instead of
SQLite. Both legs must pass for CI to go green.

---

## Conventions

Before making structural changes, read **[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)**;
it documents the durable design and the invariants you must preserve. The
highlights every contributor should internalize:

- **The store interface is the single persistence seam.** `store.Store`
  (`internal/store/store.go`) is the only persistence contract; both
  `*SQLiteStore` and `*PostgresStore` satisfy it.
- **Consumer-defined interfaces (no import cycles).** `store` imports the domain
  packages; the domain packages **never** import `store`. Each consumer defines
  its own small interface (e.g. `jobs.RunnerStore`, `notify.DispatchStore`) that
  the concrete store types satisfy structurally. When you add a store method,
  add it to `Store` *and* to any consumer interface that needs it; never make a
  domain package import `store`.
- **Dual-backend parity.** SQLite and Postgres must behave identically, differing
  only where the SQL dialect forces it (placeholders, `RETURNING`). Column-list
  constants are read by ordinal position and must match the column order in
  *both* `migrations/sqlite/*.sql` and `migrations/postgres/*.sql`. If you touch
  one backend or one migration, touch both, and the Postgres test leg will keep
  you honest.
- **Leak-free-by-construction DTOs.** The REST API and dashboard never serialize
  domain types directly. Every wire shape is a purpose-built DTO with no field
  for secret material (no token, no `*_hash`, no ciphertext). Don't add secret
  fields to DTOs; there should be structurally nowhere to put them.

When in doubt, follow the patterns already in the package you're editing, and
keep secret material off the network and out of logs.

---

## Pull request flow

1. **Branch.** Create a feature branch off `master` (e.g.
   `fix/heartbeat-timezone` or `feat/slack-channel`). Don't commit to `master`
   directly.
2. **Conventional Commits.** Use [Conventional Commit](https://www.conventionalcommits.org/)
   messages; the release changelog is generated from them by goreleaser, which
   groups `feat:` and `fix:` and drops `docs:/test:/chore:/ci:/build:/style:/refactor:`
   noise. Examples:
   - `feat(notify): add Discord channel provider`
   - `fix(store): match timestamp ordering on Postgres`
   - `docs: clarify heartbeat grace window`
   Use `feat!:` / `fix!:` (or a `BREAKING CHANGE:` footer) for breaking changes.
3. **Keep PRs small and focused.** One logical change per PR is much easier to
   review and merge than a large mixed bag.
4. **Tests are required.** Add or update tests for any behavior change. If it
   touches the store, make sure it passes on **both** SQLite and Postgres.
5. **CI must pass.** Your PR must be green: gofmt, `go vet`, `go build`, and
   `go test ./... -race` on both SQLite and Postgres. Run them locally first
   (see above).
6. **Update docs** when you change user-facing behavior, config, or
   architecture (`README.md`, `docs/`).
7. **Open the PR** against `master` and fill out the pull request template.

---

## Contributor License Agreement (CLA)

tend is licensed under **AGPL-3.0**, and contributions require signing the
project **CLA** ([`CLA.md`](CLA.md)).

On your **first pull request**, the **CLA-assistant bot** will automatically
post a comment asking you to sign. Just reply as instructed (or post the
sign-off statement from `CLA.md`); your signature is recorded in
`signatures/cla.json` and you won't be asked again. The PR's CLA status check
must be green before it can be merged.

By signing, you grant the maintainer (the copyright holder) the rights described
in `CLA.md`, including the **right to relicense** your contributions (for
example, to offer a commercial license alongside the open-source AGPL license).
Please read `CLA.md` before signing.

> The maintainer and common automation accounts are pre-allowlisted and don't
> need to sign.

---

## Code of Conduct

This project follows a [Code of Conduct](CODE_OF_CONDUCT.md). By participating,
you agree to uphold it. Please report unacceptable behavior to the contact
listed there.

## Reporting security issues

**Do not** open a public issue for a security vulnerability. See
[`SECURITY.md`](SECURITY.md) for the private reporting process.

---

Thanks again, and happy contributing!
