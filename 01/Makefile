GO := scripts/go
TESTDB := scripts/test-db.sh

.PHONY: test test-db
test-db:
	$(TESTDB)

# Capture the script output first so a failure aborts before eval.
test:
	out=$$($(TESTDB)) && eval "$$out" && $(GO) test -race ./...

REGISTRY := ghcr.io/braveantony
PLATFORM := linux/amd64
PODMAN := podman
# Rush Hour image tag; bump on every change (never "latest").
RUSH_HOUR_VERSION := 0.1.0

.PHONY: images push
images:
	$(PODMAN) build --platform $(PLATFORM) -f app/Containerfile --build-arg VERSION=v1 -t $(REGISTRY)/koupa:v1 .
	$(PODMAN) build --platform $(PLATFORM) -f app/Containerfile --build-arg VERSION=v2 -t $(REGISTRY)/koupa:v2 .
	$(PODMAN) build --platform $(PLATFORM) -f rush-hour/Containerfile -t $(REGISTRY)/rush-hour:$(RUSH_HOUR_VERSION) .

push:
	$(PODMAN) push $(REGISTRY)/koupa:v1
	$(PODMAN) push $(REGISTRY)/koupa:v2
	$(PODMAN) push $(REGISTRY)/rush-hour:$(RUSH_HOUR_VERSION)
