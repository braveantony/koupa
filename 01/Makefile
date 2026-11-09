GO := scripts/go
TESTDB := scripts/test-db.sh

.PHONY: test test-db
test-db:
	$(TESTDB)

# Capture the script output first so a failure aborts before eval.
test:
	out=$$($(TESTDB)) && eval "$$out" && $(GO) test -race ./...

REGISTRY := registry.gitlab.com/kubeantony
PLATFORM := linux/amd64
PODMAN := podman

.PHONY: images push
images:
	$(PODMAN) build --platform $(PLATFORM) -f app/Containerfile --build-arg VERSION=v1 -t $(REGISTRY)/koupa:v1 .
	$(PODMAN) build --platform $(PLATFORM) -f app/Containerfile --build-arg VERSION=v2 -t $(REGISTRY)/koupa:v2 .

push:
	$(PODMAN) push $(REGISTRY)/koupa:v1
	$(PODMAN) push $(REGISTRY)/koupa:v2
