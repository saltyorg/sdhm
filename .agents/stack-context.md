# Stack Context

Generated: 2026-09-01

## Stack
- **Language**: Go 1.25.5
- **Framework**: Cobra CLI, standard-library HTTP health endpoint, Moby Docker SDK
- **Build**: Go toolchain through Make
- **Test**: `make test` using the standard `testing` package
- **Lint**: `go vet` through `make vet` (CI gate: yes); optional golangci-lint target (CI gate: no)
- **Format**: `gofmt` through `make fmt` (CI gate: no)

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
- Run `make test`
- Run `make vet`
- Cross-compile Linux binaries after the prepare job passes
