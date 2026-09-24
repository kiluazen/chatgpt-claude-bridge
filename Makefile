.PHONY: check e2e deploy

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test ./...

# End-to-end tests against the real Claude Code and Codex; uses your Claude plan.
e2e:
	go test -tags e2e -count=1 -v ./e2e

deploy:
	scripts/deploy.sh
