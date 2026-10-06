# Contributing

## Build, test, lint

```
make build
make test
make race
make lint
make check
```

`make check` runs both `test` and `lint`, so run it before you open a pull request. `lint` runs `gofmt`, `go vet`, and `golangci-lint`, and `race` runs the tests with the Go race detector.

## Commits

We use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `chore:`, and so on), and the commit type decides the release: `feat` bumps the minor version, `fix` and `perf` bump the patch, and a `BREAKING CHANGE` footer bumps the major. `chore`, `docs`, and the other types don't create a tag or a release by themselves.

## Branches

Features and fixes go on their own branch and get merged through a pull request. Only routine maintenance goes straight to `main`.

## Releases

You don't need to do anything to release. Every push to `main` runs the release workflow, which tags a new version from the commits since the last tag (if there's something worth releasing), builds the `linux/amd64` and `linux/arm64` binaries with GoReleaser, and publishes a GitHub Release with the tarballs and container images.

## Reporting issues

Open an issue on the [issue tracker](https://github.com/khaosdoctor/proton-drive-linux-fs/issues) with your distro, the command you ran, and, if it's about the mount, the relevant lines from `journalctl --user -t proton-drive-fs` or the mount log (check [Troubleshooting](troubleshooting.md#read-the-logs)).
