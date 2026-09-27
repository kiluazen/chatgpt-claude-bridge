.PHONY: check e2e install install-bridge install-router install-switch

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race ./...

# End-to-end tests of the bridge against the real Claude Code and Codex, on
# port 41421. They use your Claude plan.
e2e:
	go test -tags e2e -count=1 -v ./bridge/e2e

# Builds both services and (re)starts them as launchd agents once idle, and
# installs the codex-bridge on/off switch.
install: check install-bridge install-router install-switch

install-bridge:
	scripts/install.sh bridge

install-router:
	scripts/install.sh router

install-switch:
	install -d $(HOME)/.local/bin
	install -m 755 scripts/codex-bridge $(HOME)/.local/bin/codex-bridge
