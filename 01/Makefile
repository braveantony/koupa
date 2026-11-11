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
SKOPEO := skopeo
# koupa versions (KOUPA_REPO, KOUPA_V1_TAG, KOUPA_V2_TAG): images.env, the
# single source of truth. Tags are semver and never overwritten.
include images.env
# Rush Hour image tag; bump on every change (never "latest", never reused).
RUSH_HOUR_VERSION := 0.1.0

.PHONY: images publish retag-history push-rush-hour
# Local builds from the working tree (nothing is pushed). The full tag goes
# into the binary: /koupa --version and /api/whoami show it.
images:
	$(PODMAN) build --platform $(PLATFORM) -f app/Containerfile --build-arg VERSION=$(KOUPA_V1_TAG) -t $(KOUPA_REPO):$(KOUPA_V1_TAG) .
	$(PODMAN) build --platform $(PLATFORM) -f app/Containerfile --build-arg VERSION=$(KOUPA_V2_TAG) -t $(KOUPA_REPO):$(KOUPA_V2_TAG) .
	$(PODMAN) build --platform $(PLATFORM) -f rush-hour/Containerfile -t $(REGISTRY)/rush-hour:$(RUSH_HOUR_VERSION) .

# Build both koupa lines from a clean export of HEAD and push them; refuses
# when a tag already exists on the registry.
publish:
	scripts/publish-images.sh

# One-off: tag the earlier koupa digests v1.0.0/v2.0.0/v1.1.0/v2.1.0.
retag-history:
	scripts/publish-images.sh --retag-history

# Rush Hour, the same way: clean export of HEAD, refuses an existing tag or
# one whose absence cannot be proven; bump RUSH_HOUR_VERSION first.
push-rush-hour:
	scripts/publish-images.sh --rush-hour $(RUSH_HOUR_VERSION)
