GO := scripts/go
TESTDB := scripts/test-db.sh

.PHONY: test test-db
test-db:
	$(TESTDB)

# Capture the script output first so a failure aborts before eval.
test:
	out=$$($(TESTDB)) && eval "$$out" && $(GO) test -race ./...
