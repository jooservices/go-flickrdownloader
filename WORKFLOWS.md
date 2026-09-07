# Workflow reference

All jobs use GitHub-hosted `ubuntu-latest` runners.

| Workflow | Trigger | Purpose |
| --- | --- | --- |
| `ci.yml` | Push and pull request on `master` or `develop` | Format, vet, race tests, vulnerability scan, and Windows build |
| `release.yml` | Tag `v*` | Build and publish GitHub Release assets |
