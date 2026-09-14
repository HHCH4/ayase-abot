.PHONY: test test-race vet web-check capability-gate capability-gate-smoke ci

test:
	GOCACHE=/tmp/abot-go-cache go test ./... -count=1

test-race:
	GOCACHE=/tmp/abot-go-cache go test -race ./... -count=1

vet:
	GOCACHE=/tmp/abot-go-cache go vet ./...

web-check:
	cd web && npm run typecheck && npm run build

# CAPABILITY_GATE_INPUT must point to a JSON document containing candidate,
# optional baseline profiles, and policy. The command exits 1 for a valid
# report that fails the policy, and 2 for malformed input or execution errors.
capability-gate:
	@test -n "$(CAPABILITY_GATE_INPUT)" || (echo "CAPABILITY_GATE_INPUT is required" >&2; exit 2)
	GOCACHE=/tmp/abot-go-cache go run ./cmd/abot-capability-gate -input "$(CAPABILITY_GATE_INPUT)"

# The checked-in profile is a deterministic schema-and-wiring smoke fixture.
# Release pipelines should replace it with provider-generated candidate data
# when live catalog evidence is available; this target never contacts a
# provider or treats the fixture as production capability evidence.
capability-gate-smoke:
	GOCACHE=/tmp/abot-go-cache go run ./cmd/abot-capability-gate -input ci/capability-gate.smoke.json >/dev/null

# ci is the repository's local/hosted quality contract. Keep the provider-free
# capability smoke gate in the same contract so changes cannot silently detach
# the release gate from the normal test pipeline.
ci: test test-race vet web-check capability-gate-smoke
