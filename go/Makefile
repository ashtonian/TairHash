# Build, test and benchmark the Go driver and fieldhash backends.
#
#   make valkey          build the pinned Valkey into .cache
#   make module          build the TairHash module (MODULE_MODE=scan|sort|slab) from MODULE_SRC
#   make test            unit tests only (no server)
#   make test-integration  start a throwaway server with the module and run every test
#   make bench OUT=dir   full benchmark (see scripts/bench.sh)
#   make report OUT=dir  re-render OUT/report.md from OUT/results.jsonl

VALKEY_VERSION ?= 9.1.3
CACHE          := .cache
VALKEY_DIR     := $(CACHE)/valkey-$(VALKEY_VERSION)
VALKEY_SERVER  := $(VALKEY_DIR)/src/valkey-server
VALKEY_CLI     := $(VALKEY_DIR)/src/valkey-cli

# TairHash source tree the module is built from. Point this at another checkout
# to benchmark a different module revision.
MODULE_SRC  ?= ..
MODULE_MODE ?= scan
MODULE_DIR  := $(CACHE)/module-$(MODULE_MODE)
MODULE_SO   := $(abspath $(MODULE_DIR))/tairhash_module.so
# CMake pins a build tree to one source directory, so each MODULE_SRC gets its own.
MODULE_BUILD := $(MODULE_DIR)/build-$(shell printf %s "$(abspath $(MODULE_SRC))" | cksum | cut -d' ' -f1)

TEST_PORT ?= 26999
OUT       ?= bench/results/$(shell date -u +%Y%m%dT%H%M%SZ)

ifeq ($(MODULE_MODE),sort)
MODULE_CMAKE_FLAGS := -DSORT_MODE=ON
else ifeq ($(MODULE_MODE),slab)
MODULE_CMAKE_FLAGS := -DSLAB_MODE=ON
else ifneq ($(MODULE_MODE),scan)
$(error MODULE_MODE must be scan, sort or slab)
endif

.PHONY: valkey module test test-integration lint bench report clean

valkey: $(VALKEY_SERVER)

$(VALKEY_SERVER):
	mkdir -p $(CACHE)
	test -d $(VALKEY_DIR) || git clone --depth 1 --branch $(VALKEY_VERSION) https://github.com/valkey-io/valkey.git $(VALKEY_DIR)
	$(MAKE) -C $(VALKEY_DIR) BUILD_TLS=no

# The module's CMakeLists writes into $(MODULE_SRC)/lib whatever the mode, so the
# result is copied to a per-mode path. CMAKE_POLICY_VERSION_MINIMUM lets CMake 4
# configure trees that still declare cmake_minimum_required(VERSION 3.0). Frame
# pointers keep server profiles walkable through module frames on every platform.
module:
	cmake -S $(MODULE_SRC) -B $(MODULE_BUILD) -DCMAKE_BUILD_TYPE=Release \
		-DCMAKE_POLICY_VERSION_MINIMUM=3.5 -DCMAKE_C_FLAGS=-fno-omit-frame-pointer $(MODULE_CMAKE_FLAGS)
	cmake --build $(MODULE_BUILD) -j
	cp $(MODULE_SRC)/lib/tairhash_module.so $(MODULE_SO)
	git -C $(MODULE_SRC) rev-parse HEAD > $(MODULE_DIR)/source-revision
	git -C $(MODULE_SRC) status --porcelain >> $(MODULE_DIR)/source-revision

test:
	go test -race ./...

test-integration: $(VALKEY_SERVER)
	@test -f $(MODULE_SO) || $(MAKE) module
	@dir=$$(mktemp -d); \
	scripts/valkey-server.sh $(TEST_PORT) $$dir --loadmodule $(MODULE_SO) & pid=$$!; \
	trap "kill $$pid 2>/dev/null; wait $$pid 2>/dev/null; rm -rf $$dir" EXIT; \
	for i in $$(seq 100); do $(VALKEY_CLI) -p $(TEST_PORT) PING >/dev/null 2>&1 && break; sleep 0.1; done; \
	VALKEY_ADDR=127.0.0.1:$(TEST_PORT) go test -race -count=1 ./...

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	shellcheck scripts/*.sh

bench: $(VALKEY_SERVER)
	@test -f $(MODULE_SO) || $(MAKE) module
	MODULE_SO=$(MODULE_SO) scripts/bench.sh $(OUT) $(BENCH_FLAGS)

report:
	go run ./cmd/thbench report $(OUT)/results.jsonl > $(OUT)/report.md

clean:
	rm -rf $(CACHE)/module-* $(CACHE)/thbench
