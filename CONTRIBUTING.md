# Contributing to Sibyl

Sibyl is licensed under the GNU Affero General Public License v3.0. The maintainer, Ben Gittins, also offers Sibyl under commercial licenses. To keep both possible, every contributor signs a Contributor License Agreement (CLA) before a pull request can merge.

## Before you start

Open an issue that describes the change before you start large work. A short discussion first saves a rewrite later.

## The Contributor License Agreement

- The [Individual CLA](docs/legal/individual-cla.md) is for everyone who contributes. It grants the maintainer a license to your contribution, including the right to sublicense it under the AGPL and under commercial licenses. You keep the copyright.
- The [Corporate CLA](docs/legal/corporate-cla.md) is for a company whose employees contribute on its behalf. The company signs it once, and each employee also signs the Individual CLA.

The CLA Assistant bot comments on your first pull request with a link. Sign in with GitHub and accept the agreement there. The bot then marks the pull request as signed, and it remembers you for later pull requests.

## Checks

Run the checks for the files that you change, and say in the pull request what you ran:

- Go: `go vet ./...`, `go test -race ./...` and `golangci-lint run ./...`.
- GitHub Actions: `actionlint`.
- Containerfiles: `hadolint`.
- Shell scripts: `shellcheck`.
- Changes under `os/`: `bootc container lint`, which the arm64 build in [AGENTS.md](AGENTS.md) runs.

Fix a linter finding rather than adding an exclusion.

The project does not accept a pull request without a signed CLA.
