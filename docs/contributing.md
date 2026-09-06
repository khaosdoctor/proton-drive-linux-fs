# Contributing

## Build, test, lint

```
make build
make test
make race
make lint
make check
```

`make check` runs both `test` and `lint`; run it before opening a pull request.
`lint` runs `gofmt`, `go vet`, and `golangci-lint`. `race` runs the test suite with
the Go race detector.

## Commits

This repository uses [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `docs:`, `chore:`, and so on). Commit types drive the automatic
release: a `feat` commit bumps the minor version, `fix` and `perf` bump the patch
version, and a `BREAKING CHANGE` footer bumps the major version. `chore`, `docs`, and
other non-release types create no tag and no release on their own.

## Branches

Features and fixes go on their own branch and merge through a pull request; there is
no direct push to `main` for anything beyond routine maintenance.

## Releases

Releases are automatic. Every push to `main` runs the release workflow, which tags a
new version from the conventional commits since the last tag (when there is a
release-worthy commit), builds binaries for `linux/amd64` and `linux/arm64` with
GoReleaser, and publishes a GitHub Release with the tarballs and container images.
There is no manual release step.

## Reporting issues

Open an issue on the
[issue tracker](https://github.com/khaosdoctor/proton-drive-linux-fs/issues). Include
your distribution, the command you ran, and, if the mount daemon is involved, the
relevant lines from `journalctl --user -t proton-drive-fs` or the mount log (see
[Troubleshooting](troubleshooting.md#logs)).
