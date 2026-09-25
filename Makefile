.PHONY: check e2e install install-bridge install-router

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race ./...

# End-to-end tests of the bridge against the real Claude Code and Codex, on
# port 41421. They use your Claude plan.
e2e:
	go test -tags e2e -count=1 -v ./bridge/e2e

# Builds both services and (re)starts them as launchd agents once idle.
install: check install-bridge install-router

install-bridge:
	scripts/install.sh bridge

install-router:
	scripts/install.sh router
