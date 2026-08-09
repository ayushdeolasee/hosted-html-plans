# Build tooling for hosted-html-plans (plan.html §6, §12).
#
# `plans` is a single static Go binary, cross-compiled from this Mac for
# whatever box it ends up running on — no Docker, no runtime dependencies.

BINARY  := plans
CMD     := ./cmd/plans
DIST    := dist
LDFLAGS := -s -w

.PHONY: build cross test vet clean deploy installer check-installer release \
	install install-cli install-skills install-server \
	darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

## installer: regenerate install.sh + skill/html-plans/push-plan from their
## sources (scripts/push-plan and the SKILL.md file). Run after editing either
## of them.
installer:
	@scripts/build-installer

## check-installer: fail if the generated artifacts are stale (CI guard).
check-installer:
	@scripts/build-installer --check

## install: dev convenience — install push-plan + the HTML plans skill from this
## checkout. Pass URL=... to record the server address in the config, e.g.
## `make install URL=https://plans.<tailnet>.ts.net`. End users don't use
## this; they run `install.sh --client`.
install:
	@scripts/install $(if $(URL),--url $(URL),)

## install-cli: just the `push-plan` command on PATH.
install-cli:
	@scripts/install --cli-only $(if $(URL),--url $(URL),)

## install-skills: just the HTML plans agent skill.
install-skills:
	@scripts/install --skills-only

## install-server: dev convenience — install the locally-built binary as a
## service on THIS machine (skips the release fetch). Pass ARGS='--print' to
## dry-run, ARGS='--user' for a user-level systemd unit.
install-server: build
	@scripts/install --server $(ARGS)

## release: cut a release so `install.sh --server` has binaries to fetch.
## Usage: make release TAG=v0.1.0   (needs the gh CLI, authed)
release: cross
	@test -n "$(TAG)" || { echo "release: pass TAG=vX.Y.Z" >&2; exit 1; }
	@command -v gh >/dev/null || { echo "release: gh CLI not found" >&2; exit 1; }
	gh release create $(TAG) \
		$(DIST)/$(BINARY)-darwin-arm64 \
		$(DIST)/$(BINARY)-darwin-amd64 \
		$(DIST)/$(BINARY)-linux-amd64 \
		$(DIST)/$(BINARY)-linux-arm64 \
		--repo ayushdeolasee/hosted-html-plans \
		--title $(TAG) \
		--notes "Install: curl -fsSL https://raw.githubusercontent.com/ayushdeolasee/hosted-html-plans/main/install.sh | bash -s -- --server"

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
