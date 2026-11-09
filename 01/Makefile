GO := scripts/go

.PHONY: test test-db
test-db:
	scripts/test-db.sh

test:
	eval "$$(scripts/test-db.sh)" && $(GO) test -race ./...
