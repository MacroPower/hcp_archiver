#!/bin/bash
set -euo pipefail

go mod tidy
go generate ./...

go build -o hcp_archiver ./cmd/hcp_archiver

mkdir -p ./completion/bash
mkdir -p ./completion/fish
mkdir -p ./completion/zsh
mkdir -p ./man

./hcp_archiver completion bash > ./completion/bash/hcp_archiver.bash
./hcp_archiver completion fish > ./completion/fish/hcp_archiver.fish
./hcp_archiver completion zsh > ./completion/zsh/_hcp_archiver
./hcp_archiver man > ./man/hcp_archiver.1
