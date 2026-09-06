# Contributing

Thanks for helping improve flickrdownloader.

## Development setup

```bash
git clone https://github.com/jooservices/go-flickrdownloader.git
cd flickrdownloader
go test -race ./...
```

Go **1.25+** is required (see `go.mod`).

## Tests

Run the full suite before opening a pull request:

```bash
go vet ./...
go test -race ./...
```

Download logic references acceptance-criteria IDs (`AC-001`, `BR8`, etc.) in comments — when changing behavior in `pkg/download`, check `docs/features/robust-downloads/` for the intended semantics and update the docs if the contract changes.

## Commit messages

Use concise messages focused on **why** the change is needed, in the style of the existing history (e.g. `fix orphan verify when albums are filtered`, not `update verify.go`).

## Pull requests

- Keep diffs focused — one logical change per PR when possible.
- Include a short test plan if behavior is user-visible.
- CI must pass (`test` workflow: vet, race tests, govulncheck).
