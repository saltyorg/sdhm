# Stack Context

Generated: 2026-09-01

## Stack
- **Language**: Go 1.27.0
- **Framework**: Cobra CLI, standard-library HTTP health endpoint, Moby Docker SDK
- **Build**: Go toolchain through Make (`make build`)
- **Test**: `make test` and `make test-race` using the standard `testing` package
- **Static analysis**: `go vet` through `make vet`
- **Format**: `gofmt` through `make fmt` and non-mutating `make fmt-check`
- **Module hygiene**: non-mutating `go mod tidy -diff` through `make tidy-check`

## Secondary Languages
- YAML (GitHub Actions build and release workflow)
- Make (local build, test, verification, install, and maintenance targets)
- Markdown (user and contributor documentation)

## Conventions
- Error handling: explicit returns with contextual `%w` wrapping
- Module structure: top-level domain packages; `cmd/sdhm` owns production wiring
- Naming: idiomatic exported Go identifiers with package-local implementation helpers
- Tests: colocated `_test.go` files, primarily table-driven and temporary-filesystem based

## CI Gates
- Download module dependencies
- Run `make check`, which executes `fmt-check`, `tidy-check`, race-enabled tests, `vet`, and `build`
- Cross-compile the unchanged Linux artifact matrix only after `make check` passes
