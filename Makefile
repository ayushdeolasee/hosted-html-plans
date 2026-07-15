# Build tooling for hosted-html-plans (plan.html §6, §12).
#
# `plans` is a single static Go binary, cross-compiled from this Mac for
# whatever box it ends up running on — no Docker, no runtime dependencies.

BINARY  := plans
CMD     := ./cmd/plans
DIST    := dist
LDFLAGS := -s -w

.PHONY: build cross test vet clean deploy installer check-installer \
	install install-cli install-skills \
	darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

## installer: regenerate install.sh + skill/push-plan/push-plan from
## scripts/push-plan (the single source of truth). Run after editing it.
installer:
	@scripts/build-installer

## check-installer: fail if the generated artifacts are stale (CI guard).
check-installer:
	@scripts/build-installer --check

## install: dev convenience — install push-plan + both skills from this
## checkout. Pass URL=... to record the server address in the config, e.g.
## `make install URL=https://plans.<tailnet>.ts.net`. End users don't use
## this; they curl install.sh and `npx skills add push-plan`.
install:
	@scripts/install $(if $(URL),--url $(URL),)

## install-cli: just the `push-plan` command on PATH.
install-cli:
	@scripts/install --cli-only $(if $(URL),--url $(URL),)

## install-skills: just the Claude Code skills.
install-skills:
	@scripts/install --skills-only

## build: compile for the current host (GOOS/GOARCH from your environment).
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

## cross: build all four release targets into dist/.
cross: darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

darwin/arm64:
	mkdir -p $(DIST)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-arm64 $(CMD)

darwin/amd64:
	mkdir -p $(DIST)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-amd64 $(CMD)

linux/amd64:
	mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 $(CMD)

linux/arm64:
	mkdir -p $(DIST)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64 $(CMD)

## test: run the full test suite (+ verify generated artifacts aren't stale).
test: check-installer
	go test ./...

## vet: static analysis.
vet:
	go vet ./...

## clean: remove build artifacts.
clean:
	rm -rf $(DIST) $(BINARY)

## deploy: example — copy the Linux binary to your box and (re)install the
## service there over SSH. Adjust BOX (a host or Tailscale/SSH-config alias)
## and the remote path, then run `make deploy BOX=hermes`.
#
# BOX ?= hermes
# deploy: linux/amd64
# 	scp $(DIST)/$(BINARY)-linux-amd64 $(BOX):~/plans
# 	ssh $(BOX) 'chmod +x ~/plans && sudo ~/plans service install'
