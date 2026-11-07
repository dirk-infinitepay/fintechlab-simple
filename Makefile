# fintechlab-simple — local compose lab
SHELL := /bin/sh

COMPOSE ?= $(shell if docker compose version >/dev/null 2>&1; then echo "docker compose"; \
	elif podman compose version >/dev/null 2>&1; then echo "podman compose"; \
	elif command -v docker-compose >/dev/null 2>&1; then echo "docker-compose"; \
	else echo "docker compose"; fi)

# Plain single-container engine (not compose) for `make e2e-docker`,
# which attaches one container to the already-running compose network
# instead of being a compose service itself — compose "profiles" would be
# the normal way to keep a batch job out of `make up`, but podman-compose
# (a real target here) does not implement profiles at all, so an e2e
# compose service would either always run on `up` or need profiles it
# can't see. A plain `run --network` sidesteps that entirely.
ENGINE ?= $(shell if docker version >/dev/null 2>&1; then echo docker; \
	elif podman version >/dev/null 2>&1; then echo podman; \
	else echo docker; fi)

# podman-compose 1.x does not implement Compose "profiles".
COMPOSE_IS_PODMAN := $(findstring podman,$(COMPOSE))

.PHONY: help keys-migrate keys-regenerate certs sftp-dirs key-dirs console-dir b4b-dir up start down logs test vet fmt-check demo-payment console e2e e2e-docker tidy rebuild pod-images pod-up pod-down pod-bundle

help:
	@echo "make up             build, replace the containers, start the lab"
	@echo "make console        open the control panel (http://127.0.0.1:8090)"
	@echo "make demo-payment   create a payment and wait for the webhook"
	@echo "make e2e            run the end-to-end scenarios locally (go run)"
	@echo "make e2e-docker     build+run the e2e suite as one container against the compose network"
	@echo "make test           unit tests + vet + gofmt check (no Docker required)"
	@echo "make start          bring the lab back up without rebuilding"
	@echo "make down           compose down"
	@echo "make keys-regenerate new PKI and keypairs in LAB_KEYS_DIR ($(LAB_KEYS_DIR)), lab restarted"
	@echo "make rebuild        no-cache rebuild of the lab-api and console images"
	@echo ""
	@echo "make pod-images     build every image, tagged $(IMAGE_PREFIX)/<service>:$(LAB_VERSION)"
	@echo "make pod-up         run the whole lab as one podman pod (podman kube play);"
	@echo "                    REGENERATE_KEYS=true to start it on new keys"
	@echo "make pod-down       stop the pod; its volumes (keys, data) are kept"
	@echo "make pod-bundle     images + manifest in dist/, to run on a machine with no checkout"

# Where the lab generates every key and certificate, one subdirectory per
# owner (keys/README.md says what each file is for):
#   certs/        the lab CA and the TLS certificates it signs (ca/generate.sh)
#   b4b-keys/     B4B's RS512 JWT keypair (the b4b service generates it)
#   wlsftp-keys/  Worldline's SSH host key and PGP keypair (worldline generates them)
# Containers see it at /keys whatever it is on the host.
#
# Set it to keep the keys outside the checkout: `export LAB_KEYS_DIR=...`
# before make, or on the command line. A platform that reads the lab's
# keys from disk points at the same directory -- buddy's local runner takes
# it as FINTECH_SIM_LAB, and the subdirectory names above are the ones it
# reads. Made absolute, because compose would take a bare relative name for
# a named volume, and exported, so compose and the scripts see it.
LAB_KEYS_DIR ?= keys
override LAB_KEYS_DIR := $(abspath $(LAB_KEYS_DIR))
export LAB_KEYS_DIR

# The three subdirectories used to sit at the top of the checkout. They are
# moved, not regenerated: the platform under test may already hold copies
# (the B4B signing key, the PGP keys, the pinned SFTP host key), and a
# fresh set would fail its next call somewhere far from here. A directory
# that exists in both places is left alone and reported, not merged.
keys-migrate:
	@for d in certs b4b-keys wlsftp-keys; do \
		new="$(LAB_KEYS_DIR)/$$d"; \
		if [ -d "$$d" ] && [ ! -e "$$new" ]; then \
			mkdir -p "$(LAB_KEYS_DIR)" && mv "$$d" "$$new" && echo "moved $$d/ -> $$new/"; \
		elif [ -d "$$d" ] && [ "$$(cd "$$d" && pwd)" != "$$new" ]; then \
			echo "both $$d/ and $$new/ exist; using $$new/ -- delete $$d/ once you have checked it"; \
		fi; \
	done

# Throw every key and certificate away and have the lab issue new ones:
# stop the lab, empty the three directories, a new PKI, start the lab again
# (b4b and worldline generate theirs on start; settlement and the console
# wait for them). Every copy a platform holds stops working, hence the
# question -- CONFIRM=yes answers it for scripts. down and up rather than
# `start`, because docker compose's `up -d` alone would leave the running
# containers holding the old keys in memory.
keys-regenerate:
	@if [ "$(CONFIRM)" != yes ]; then \
		printf 'Delete every key and certificate in %s and issue new ones?\nA platform holding copies of the old ones stops working. [yes/N] ' "$(LAB_KEYS_DIR)"; \
		read answer; [ "$$answer" = yes ] || { echo "nothing changed"; exit 1; }; \
	fi
	$(COMPOSE) down
	rm -f "$(LAB_KEYS_DIR)"/certs/* "$(LAB_KEYS_DIR)"/b4b-keys/* "$(LAB_KEYS_DIR)"/wlsftp-keys/*
	@$(MAKE) --no-print-directory certs key-dirs sftp-dirs console-dir b4b-dir
	$(COMPOSE) up -d
	@echo ""
	@echo "new keys in $(LAB_KEYS_DIR). Restart the platform's runner, and download its .env again"
	@echo "if it was configured from one (Configuration -> Connect your platform)."

certs: keys-migrate
	@mkdir -p "$(LAB_KEYS_DIR)/certs"
	@CERTS_DIR="$(LAB_KEYS_DIR)/certs" ./ca/generate.sh

# Host directories shared into settlement/worldline as bind mounts (so a
# human can `ls sftp/out` directly). Pre-created here, world-writable: the
# two containers run as different non-root UIDs (65532) that don't own
# whatever UID podman/docker would otherwise auto-create these as, and
# unlike keys/ these hold no secrets — same fake-and-obvious-lab tradeoff
# as ca/generate.sh's cert permissions (see docs/security/ca-and-tls.md).
# Non-recursive: subdirectories/files the containers create underneath are
# their own responsibility (see internal/sftp's Stage(), which already
# creates 0o777) — recursing here would try to chmod paths a container UID
# owns, which the host user can't do and isn't root, failing the whole
# target on every `make up` after the first.
sftp-dirs:
	@mkdir -p sftp/out sftp/staging sftp/outbound sftp/inbound sftp/config sftp/archive
	@chmod 777 sftp sftp/out sftp/staging sftp/outbound sftp/inbound sftp/config sftp/archive

# Same rationale as sftp-dirs, for the two generated-keypair directories
# (docs/ARCHITECTURE-vendor-corrections.md Addendum sections D/E): b4b
# generates its JWT keypair into b4b-keys/, settlement reads it back to
# sign outgoing calls; worldline generates its SSH host key + PGP keypair
# into wlsftp-keys/, and settlement and the e2e suite read the PGP private
# key back to decrypt what they download over the real SFTP+PGP channel.
# World-writable, non-secret (lab-only, gitignored, never real key material)
# -- same tradeoff as certs/.
key-dirs: keys-migrate
	@mkdir -p "$(LAB_KEYS_DIR)/b4b-keys" "$(LAB_KEYS_DIR)/wlsftp-keys"
	@chmod 777 "$(LAB_KEYS_DIR)/b4b-keys" "$(LAB_KEYS_DIR)/wlsftp-keys"

# The console's merchant registry. A host bind mount rather than a named
# volume (unlike settlement-data) because merchants you create in the UI
# are lab data a human wants to read, diff and delete -- `cat
# console-data/registry.json` is the whole state. Same world-writable
# tradeoff as the directories above: the container runs as UID 65532.
console-dir:
	@mkdir -p console-data
	@chmod 777 console-data

# B4B's own state: every company it has boarded, its people, their extended
# profile, the beneficiaries and the payments. Same bind-mount-and-777
# tradeoff as console-data, and for the same reason -- `cat
# b4b-data/state.json` is the whole of what the payout rail remembers, and
# a boarded merchant that vanishes on restart makes the rail untestable.
b4b-dir:
	@mkdir -p b4b-data
	@chmod 777 b4b-data

# Why `up` tears down first under podman.
#
# podman-compose 1.x does not replace a container when its image changes:
# `up --build` builds a fresh binary and then keeps serving the old one. The
# failure is silent and it points somewhere else — a request body or a
# config field looks wrong when the truth is that the parser is older than
# the thing it is parsing.
#
# --force-recreate is not the fix. It removes and creates in one pass and
# races itself ("container name is already in use"), and it cannot replace a
# service others depend on ("has dependent containers which must be removed
# before it"). Both failures are noisy and partial, which is worse than
# either working or not.
#
# So: build, down, up. It costs a few seconds — the image layers are cached,
# only the containers are replaced — and it always runs what you just built.
# Named volumes survive `down`, so settlement-data and the console registry
# are not lost.
ifeq ($(COMPOSE_IS_PODMAN),podman)
COMPOSE_UP = $(COMPOSE) build && $(COMPOSE) down && $(COMPOSE) up -d
else
COMPOSE_UP = $(COMPOSE) up -d --build
endif

up: certs sftp-dirs key-dirs console-dir b4b-dir
	$(COMPOSE_UP)
	@echo "lab is up. payment-api :8080  bank :8081  notifier :8082  receiver :8443"
	@echo "         settlement :8083  worldline :8084 (+ SFTP+PGP :2222)  banking-circle :8085 (+ internal :8095)  b4b :8086  aci :8087  verify :8088  clock :8096"
	@echo ""
	@echo "control panel: http://127.0.0.1:8090   (make console)"
	@echo "lab API:       http://127.0.0.1:8097/v1 (docs/api/lab-api.md)$(if $(LAB_API_TOKEN), -- asks for LAB_API_TOKEN)"
	@echo "next: make demo-payment  (or: make e2e)"


# The console embeds its web assets, and the lab API serves the routes the
# page calls, so the two are rebuilt together. --no-cache is for when a warm
# layer cache means `up --build` quietly reuses the old ones.
rebuild:
	$(COMPOSE) build --no-cache lab-api console
	@echo "rebuilt lab-api and console (no cache). next: make up"

# start is `up` without the build step: it still replaces the containers,
# so the lab reaches a known state, but it does not recompile anything. Use
# it when you have changed no Go code and just want the stack back.
#
# It tears down first for the same reason `up` does — `up -d` against a
# stack that is already running reports "container already exists" once per
# service and changes nothing, which reads like a failure and is not.
ifeq ($(COMPOSE_IS_PODMAN),podman)
COMPOSE_START = $(COMPOSE) down && $(COMPOSE) up -d
else
COMPOSE_START = $(COMPOSE) up -d
endif

start: certs sftp-dirs key-dirs console-dir b4b-dir
	$(COMPOSE_START)
	@echo "lab restarted from the images already built. Use 'make up' after a code change."

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=100

test: vet fmt-check
	go test ./...

vet:
	go vet ./...

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

tidy:
	go mod tidy

demo-payment:
	@./scripts/demo-payment.sh

# Best-effort open; prints the URL either way, since a headless or remote
# box has no browser to hand it to.
console:
	@echo "http://127.0.0.1:8090"
	@(xdg-open http://127.0.0.1:8090 >/dev/null 2>&1 || open http://127.0.0.1:8090 >/dev/null 2>&1 || true)

# --- one podman pod -----------------------------------------------------
# The same lab as one `podman kube play`, from images rather than a
# checkout: docs/deploy-pod.md. The manifest names every image
# localhost/fintechlab/<service>:v1 and is a runnable file as it stands;
# a different IMAGE_PREFIX or LAB_VERSION is rewritten on the way in, so
# pushing to a registry never means editing it.

LAB_VERSION ?= v1
IMAGE_PREFIX ?= localhost/fintechlab
POD_MANIFEST := deploy/pod/fintechlab.yaml
# ca is the pod's init container: it lays out the volumes and issues the PKI.
POD_SERVICES := ca clock bank notifier payment-api receiver settlement worldline banking-circle b4b aci verify verification lab-api console
POD_RENDER = sed -e 's|localhost/fintechlab/\([a-z-]*\):v1|$(IMAGE_PREFIX)/\1:$(LAB_VERSION)|' $(POD_MANIFEST)

pod-images:
	@for s in $(POD_SERVICES); do \
		echo "building $(IMAGE_PREFIX)/$$s:$(LAB_VERSION)"; \
		podman build -q -f docker/Dockerfile.$$s -t $(IMAGE_PREFIX)/$$s:$(LAB_VERSION) . >/dev/null || exit 1; \
	done

# --replace: a second pod-up swaps the containers for the current images.
# The named volumes (keys, SFTP tree, state) are not part of that, so the
# PKI a platform was configured with survives a redeploy.
# Played from deploy/pod: kube play looks in its working directory for a
# directory named like each image, to build it -- and podman 4.9 does so
# even with --build=false. The repo root has a b4b binary and a worldline
# directory, and the play fails on them. Nothing in deploy/pod is so named.
#
# REGENERATE_KEYS=true passes deploy/pod/regenerate-keys.yaml to the play:
# the init container discards the keys volume's contents and the pod starts
# on a new PKI and new keypairs.
REGENERATE_KEYS ?= false
POD_PLAY_OPTS = $(if $(filter true,$(REGENERATE_KEYS)),--configmap regenerate-keys.yaml)

# LAB_API_TOKEN=… has the lab API ask every caller for it, as under compose.
# The pod reads it from the ConfigMap deploy/pod/lab-api-token.yaml
# defines; pod-up appends one, holding the token, to the manifest it pipes
# to the play, so the token is never written to a file. YAML single quotes:
# a ' in the token doubles.
POD_TOKEN_CONFIGMAP = if [ -n "$$LAB_API_TOKEN" ]; then \
	printf '%s\n' '---' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: fintechlab-lab-api-token' 'data:'; \
	printf "  lab-api-token: '%s'\n" "$$(printf '%s' "$$LAB_API_TOKEN" | sed "s/'/''/g")"; \
	fi

pod-up:
	@{ $(POD_RENDER); $(POD_TOKEN_CONFIGMAP); } | (cd deploy/pod && podman kube play --replace --build=false $(POD_PLAY_OPTS) -)
	@echo ""
	@echo "lab is up as pod 'fintechlab'. control panel: http://127.0.0.1:8090"
	@echo "lab API: http://127.0.0.1:8097/v1$(if $(LAB_API_TOKEN), (asks for LAB_API_TOKEN))"
	@echo "platform env + keys: Configuration -> Connect your platform"

# Keeps the volumes, keys included. New keys are `make pod-up
# REGENERATE_KEYS=true`, not a teardown.
pod-down:
	$(POD_RENDER) | (cd deploy/pod && podman kube down -)

# Everything a machine without this checkout needs, in dist/:
#   podman load -i fintechlab-$(LAB_VERSION)-images.tar
#   podman kube play --replace fintechlab-$(LAB_VERSION).yaml
#   (add --configmap regenerate-keys.yaml to issue new keys, and
#    --configmap lab-api-token.yaml, with a token in it, to ask for one)
pod-bundle: pod-images
	@mkdir -p dist
	podman save -m -o dist/fintechlab-$(LAB_VERSION)-images.tar $(foreach s,$(POD_SERVICES),$(IMAGE_PREFIX)/$(s):$(LAB_VERSION))
	@$(POD_RENDER) > dist/fintechlab-$(LAB_VERSION).yaml
	@cp deploy/pod/regenerate-keys.yaml deploy/pod/lab-api-token.yaml dist/
	@echo "dist/fintechlab-$(LAB_VERSION)-images.tar + dist/fintechlab-$(LAB_VERSION).yaml -- see docs/deploy-pod.md"

e2e:
	@go run ./cmd/e2e

# The compose network the lab runs on: <project>_default.
E2E_NETWORK ?= fintechlab-simple_default

e2e-docker:
	$(ENGINE) build -f docker/Dockerfile.e2e -t fintechlab-simple_e2e .
	$(ENGINE) run --rm --network $(E2E_NETWORK) \
		-e PAYMENT_API_URL=http://payment-api:8080 \
		-e BANK_URL=http://bank:8081 \
		-e NOTIFIER_URL=http://notifier:8082 \
		-e RECEIVER_URL=https://receiver:8443 \
		-e RECEIVER_DELIVERY_URL=https://receiver:8443 \
		-e SETTLEMENT_URL=http://settlement:8083 \
		-e BANKING_CIRCLE_URL=https://banking-circle:8085 \
		-e B4B_URL=http://b4b:8086 \
		-e LAB_API_URL=http://lab-api:8097 \
		-e LAB_API_TOKEN \
		-e B4B_JWT_PRIVATE_KEY_PATH=/keys/b4b-keys/private.pem \
		-e B4B_JWT_KEY_ID=b4b-mock-1 \
		-e CA_FILE=/keys/certs/ca.pem \
		-e CLIENT_CERT=/keys/certs/client.pem \
		-e CLIENT_KEY=/keys/certs/client-key.pem \
		-e WORLDLINE_SFTP_HOST=worldline \
		-e WORLDLINE_SFTP_PASSWORD=sim-sftp-dev-only \
		-e WORLDLINE_PGP_PRIVATE_KEY_PATH=/keys/wlsftp-keys/worldline_private.asc \
		-v "$(LAB_KEYS_DIR)":/keys:ro \
		fintechlab-simple_e2e
