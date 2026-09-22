# Contributing

Thanks for taking the time.

## Ground rules

- Small, focused pull requests. One behaviour change per PR.
- Domain packages (`internal/domain/...`) stay pure: no I/O, no globals,
  deterministic output. Anything that talks to the network lives under
  `internal/adapters` behind an interface in `internal/ports`.
- Every non-trivial design decision gets an ADR in `docs/adr/`.
- Rule files under `rules/` are policy. Bump `version` when you change
  criteria; never edit a version in place after it has produced decisions.

## Workflow

```bash
make test        # go test -race
make lint        # golangci-lint
make cover       # coverage report
make run         # local, zero dependencies
./examples/demo.sh
```

CI runs build, vet, race tests, golangci-lint, govulncheck, a Docker build
and `helm lint`. All must be green.

## Commit messages

Conventional style (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`), present
tense, with a body explaining *why* when it is not obvious.
