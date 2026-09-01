# Stack Context

Generated: 2026-09-01

## Stack
- **Language**: Go 1.27.0
- **Framework**: Cobra CLI, standard-library HTTP health endpoint, Moby Docker SDK
- **Build**: Go toolchain through Make (`make build`)
- **Test**: `make test` and `make test-race` using the standard `testing` package
- **Lint**: `go vet` through `make vet`
- **Format**: `gofmt` through `make fmt` and non-mutating `make fmt-check`
- **Module hygiene**: non-mutating `go mod tidy -diff` through `make tidy-check`

## Secondary Languages
- YAML (GitHub Actions build and release workflow)
- Make (local build, test, lint, install, and maintenance targets)
- Markdown (user and contributor documentation)

## Conventions
- Error handling: explicit returns with contextual `%w` wrapping
- Module structure: domain-named packages under `internal/`; `cmd/sdhm` wires the application
- Naming: idiomatic exported Go identifiers with package-local implementation helpers
- Tests: colocated `_test.go` files, primarily table-driven and temporary-filesystem based

## CI Gates
- Download module dependencies
- Run `make fmt-check`
- Run `make tidy-check`
- Run `make test`
- Run `make test-race`
- Run `make vet`
- Run `make build`
- Cross-compile Linux binaries after the prepare job passes
