# Meridian Trellis build.
#
#   make            build bin/trellis for this machine (plus meshd/meshctl links)
#   make cross      build dist/ binaries for arm64, armv7 and amd64
#   make deb        build dist/deb/ packages: trellis (arm64, armhf, amd64; voice in arm64 and amd64) and trellis-batman-adv-dkms
#   make deb-test   install, upgrade, remove and purge the packages in Debian and Ubuntu containers
#   make deploy-test  trellis-deploy against Debian and Ubuntu containers running sshd
#   make release    dist/release/<version>/: signed packages and checksums (scripts/release.sh)
#   make publish    the release on GitHub, after checking it (scripts/publish.sh; DRY_RUN=1 to try)
#   make apt-repo APT_REPO=DIR APT_GPG_KEY=FINGERPRINT   add dist/release/<version>/'s packages to the apt repository in DIR
#   make apt-test   an apt repository signed with a throwaway key: install, upgrade, tampering, in containers
#   make deploy-tool  dist/trellis-deploy/: trellis-deploy for Linux, macOS and Windows (amd64, arm64)
#   make test       unit tests (no root)
#   make lab        lab tests in network namespaces (root, or a user namespace)
#   make check      gofmt, vet and tests
#   make fuzz       run every fuzzer for FUZZTIME (default 30s) each; "make test" runs their seeds
#   make gen        regenerate netlink constants from the kernel headers
#   make deploy     check, build and update every node in deploy.hosts
#   make lab-scale  scale lab: 50 nodes, ID convergence and gossip volume (~6 min)
#   make lab-rollout  rollout lab: a mesh-wide upgrade through 4 nodes (~3 min)
#   make nodes      show meshctl status from every node in deploy.hosts
#   make band BAND=2g [CHANNEL=6]   switch mesh-profile.yaml to a band and push it to every node
#   make profile    push mesh-profile.yaml to every node and apply it live
#   make web-password            set the web UI's admin password on every node
#   make web-wan WAN=on|off      serve the web UI on each node's uplink network too (bench), or not
#   make ui-dev     the web UI on http://127.0.0.1:8480 against made-up data (no mesh needed)

# Releases are tags named vX.Y.Z; the version drops the "v".
VERSION ?= $(shell (git describe --tags --always --dirty 2>/dev/null || echo dev) | sed 's/^v//')
GO      ?= go
LDFLAGS  = -s -w -X main.version=$(VERSION)
GOFLAGS  = -trimpath
PKG      = ./cmd/trellis
ARCHES   = arm64 arm armv6 amd64
# trellis-voiced loads libopus and libasound at run time (purego): built
# and tested on these only.
VOICE_ARCHES = arm64 amd64

.PHONY: static lab-voice lab-rollout all build cross deb deb-test apt-repo apt-test deploy-test release publish deploy-tool fuzz test lab lab-scale check fmt vet gen clean deploy nodes band profile web-password web-wan ui-dev

all: build

build:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/trellis $(PKG)
	ln -sf trellis bin/meshd
	ln -sf trellis bin/meshctl
	ln -sf trellis bin/webd
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/trellis-voiced ./cmd/trellis-voiced

cross:
	@mkdir -p dist
	@for arch in $(ARCHES); do \
	  out=dist/trellis-linux-$$arch; \
	  goarch=$$arch; \
	  if [ $$arch = arm ]; then export GOARM=7; out=dist/trellis-linux-armv7; fi; \
	  if [ $$arch = armv6 ]; then export GOARM=6; goarch=arm; fi; \
	  echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$goarch $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done
	@for arch in $(VOICE_ARCHES); do \
	  echo "building dist/trellis-voiced-linux-$$arch"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/trellis-voiced-linux-$$arch ./cmd/trellis-voiced || exit 1; \
	done
	@ls -la dist/

# Packages. Every file in them is stamped with the last commit's time, so
# the same commit gives the same packages. TRELLIS_RELEASE_KEY=<secret key
# file> signs them (tools/mkdeb keygen makes a key).
BATADV_VERSION := $(shell sed -n 's/^BATADV_VERSION=//p' packaging/dkms/source)
BATADV_URL     := $(shell sed -n 's/^BATADV_URL=//p' packaging/dkms/source)
BATADV_TARBALL  = dist/cache/batman-adv-$(BATADV_VERSION).tar.gz
EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null)
DEB_OUT ?= dist/deb

$(BATADV_TARBALL):
	@mkdir -p dist/cache
	curl -fsSL -o $@.part $(BATADV_URL)
	@mv $@.part $@

deb: cross $(BATADV_TARBALL)
	@rm -rf $(DEB_OUT)
	@for pair in arm64:arm64 armhf:armv6 amd64:amd64; do \
	  voiced=; [ -f dist/trellis-voiced-linux-$${pair##*:} ] && voiced="--voiced dist/trellis-voiced-linux-$${pair##*:}"; \
	  $(GO) run ./tools/mkdeb trellis --version '$(VERSION)' --arch $${pair%%:*} \
	    --binary dist/trellis-linux-$${pair##*:} $$voiced --out $(DEB_OUT) --epoch '$(EPOCH)' --key '$(TRELLIS_RELEASE_KEY)' $(DEB_SIGN_FLAGS) || exit 1; \
	done
	@$(GO) run ./tools/mkdeb batman-adv --source $(BATADV_TARBALL) --out $(DEB_OUT) --epoch '$(EPOCH)' --key '$(TRELLIS_RELEASE_KEY)' $(DEB_SIGN_FLAGS)

deb-test:
	scripts/deb-test.sh

deploy-test:
	scripts/deploy-test.sh

release:
	scripts/release.sh

publish:
	scripts/publish.sh

# The apt repository: a directory to serve as it is (tools/aptrepo).
APT_REPO    ?=
APT_GPG_KEY ?=
apt-repo:
	@[ -n "$(APT_REPO)" ] && [ -n "$(APT_GPG_KEY)" ] || { echo "set APT_REPO (the repository's folder) and APT_GPG_KEY (the archive key)"; exit 2; }
	$(GO) run ./tools/aptrepo --dir "$(APT_REPO)" --gpg-key "$(APT_GPG_KEY)" dist/release/$(VERSION)/*.deb

apt-test:
	scripts/apt-test.sh

# trellis-deploy runs on the operator's computer, not on a node.
DEPLOY_TOOL_OUT ?= dist/trellis-deploy
deploy-tool:
	@mkdir -p $(DEPLOY_TOOL_OUT)
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; out=$(DEPLOY_TOOL_OUT)/trellis-deploy_$(VERSION)_$${os}_$${arch}; \
	  if [ $$os = windows ]; then out=$$out.exe; fi; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(GOFLAGS) -ldflags '-s -w -X main.version=$(VERSION)' -o $$out ./cmd/trellis-deploy || exit 1; \
	  echo $$out; \
	done

test:
	$(GO) test ./...

# The parsers of untrusted input: gossip from the air, packages and
# configuration from browsers and files, profile codes from QR codes, DHCP
# from whatever is on the other end of a cable.
FUZZTIME ?= 30s
FUZZERS = gossip:FuzzOpenRecord gossip:FuzzOpenRaw gossip:FuzzOpenMessages gossip:FuzzParseLinkEntry \
	deb:FuzzRead deb:FuzzParseKeys config:FuzzParseProfile config:FuzzDecodeProfileCode config:FuzzParseNode \
	regdb:FuzzParse wpa:FuzzParseEvent dnsmasq:FuzzParseLeases version:FuzzVersion cot:FuzzMarshal \
	rollout:FuzzDecodeRollout dhcpc:FuzzParse dhcpc:FuzzUDPPayload \
	rtp:FuzzParse rtp:FuzzParseCompound voice:FuzzDeliver webd:FuzzWSRead
fuzz:
	@for f in $(FUZZERS); do \
	  echo "== $${f#*:} ($${f%%:*}, $(FUZZTIME))"; \
	  out=$$($(GO) test ./internal/$${f%%:*} -run '^$$' -fuzz "^$${f#*:}$$" -fuzztime $(FUZZTIME) 2>&1); rc=$$?; \
	  printf '%s\n' "$$out" | grep -v '^fuzz: elapsed'; \
	  [ $$rc -eq 0 ] || exit 1; \
	done

lab: build
	./lab/lab.sh

lab-scale: build
	./lab/lab.sh --scale

lab-voice: build
	./lab/voice.sh

lab-rollout: build
	./lab/rollout.sh

fmt:
	@test -z "$$(gofmt -l cmd internal lab tools 2>/dev/null)" || { gofmt -l cmd internal lab tools; echo "run gofmt -w"; exit 1; }

vet:
	$(GO) vet ./...

# The trellis binary must stay static: libopus (internal/opus) is loaded
# at run time, which links a binary dynamically.
static:
	@! $(GO) list -deps ./cmd/trellis ./cmd/trellis-deploy | grep -q 'ebitengine/purego' || { echo "cmd/trellis or cmd/trellis-deploy imports internal/opus; only the voice service may"; exit 1; }

check: fmt vet static test

gen:
	python3 -I tools/genconst/genconst.py --header /usr/include/linux/batman_adv.h --package batadv > internal/batadv/consts_gen.go
	python3 -I tools/genconst/genconst.py --header /usr/include/linux/nl80211.h --package nl80211 \
	  --enum nl80211_commands --enum nl80211_attrs --enum nl80211_iftype --enum nl80211_band_attr \
	  --enum nl80211_frequency_attr --enum nl80211_band --enum nl80211_iface_limit_attrs \
	  --enum nl80211_if_combination_attrs --enum nl80211_chan_width --enum nl80211_channel_type \
	  --enum nl80211_tx_power_setting --enum nl80211_reg_initiator --enum nl80211_reg_type \
	  --enum nl80211_reg_rule_attr --enum nl80211_reg_rule_flags --enum nl80211_dfs_regions \
	  --enum nl80211_meshconf_params --enum nl80211_mesh_setup_params --enum nl80211_sta_info \
	  --enum nl80211_rate_info --enum nl80211_mpath_info --enum nl80211_feature_flags \
	  --enum nl80211_ext_feature_index --enum nl80211_band_iftype_attr --enum nl80211_user_reg_hint_type \
	  --enum nl80211_mntr_flags --enum nl80211_plink_state --enum nl80211_mesh_power_mode \
	  --enum nl80211_survey_info --enum nl80211_bss \
	  > internal/nl80211/consts_gen.go
	gofmt -w internal/batadv/consts_gen.go internal/nl80211/consts_gen.go

clean:
	rm -rf bin dist

deploy:
	FAST='$(FAST)' DRY_RUN='$(DRY_RUN)' scripts/deploy.sh $(HOSTS)

nodes:
	scripts/deploy.sh --status $(HOSTS)

band: build
	@test -n "$(BAND)" || { echo "usage: make band BAND=2g|5g|6g [CHANNEL=N] [HOSTS=...]"; exit 2; }
	bin/meshctl profile band $(BAND) $(if $(CHANNEL),--channel $(CHANNEL)) mesh-profile.yaml
	scripts/deploy.sh --profile $(HOSTS)

profile:
	scripts/deploy.sh --profile $(HOSTS)

web-password:
	scripts/deploy.sh --web-password $(HOSTS)

web-wan:
	@test -n "$(WAN)" || { echo "usage: make web-wan WAN=on|off [HOSTS=...]"; exit 2; }
	scripts/deploy.sh --web-wan $(WAN) $(HOSTS)

# The web UI against tools/fakemeshd: a made-up seven-node mesh, password
# "trellis-demo". SETUP=1 shows the setup wizard of an unprovisioned node;
# LINKS=2 gives the made-up node a HaLow radio and a second mesh link,
# LINKS=3 a third link on 2.4 GHz as well. BARE=1 is a node with no
# per-radio rules, as freshly installed. ONE_RADIO=1 is a node with one
# dual-band adapter in a 2.4 + 5 GHz profile.
# UI files are built into the binary, so run it again after editing them.
UI_DEV_DIR ?= $(or $(XDG_RUNTIME_DIR),/tmp)/trellis-ui-dev
ui-dev: build
	$(GO) build -o bin/fakemeshd ./tools/fakemeshd
	@mkdir -p $(UI_DEV_DIR)
	@echo "== http://127.0.0.1:8480  (password: trellis-demo; Ctrl-C stops both)"
	@bin/fakemeshd --socket $(UI_DEV_DIR)/meshd.sock --console $(UI_DEV_DIR)/console.sock $(if $(SETUP),--setup) $(if $(LINKS),--links $(LINKS)) $(if $(BARE),--bare) $(if $(ONE_RADIO),--one-radio) $(if $(UNRESTRICTED),--unrestricted) $(if $(PORTAL),--portal) & fake=$$!; \
	  trap "kill $$fake 2>/dev/null" EXIT INT TERM; sleep 0.5; \
	  bin/trellis webd --socket $(UI_DEV_DIR)/meshd.sock --voice-console $(UI_DEV_DIR)/console.sock --listen 127.0.0.1:8480 --plain-http
