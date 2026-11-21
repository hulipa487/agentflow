# AgentFlow Makefile — pure-Go build (no cgo / C++ toolchain required).
#
#   make            # build the agentflow binary (default)
#   make build      # build the agentflow binary
#   make test       # go test ./...
#   make vet        # go vet ./...
#   make run        # build, then run with CONFIG (default: agentflow.yaml / config.yaml)
#   make clean      # remove built binaries and legacy build artifacts

.DEFAULT_GOAL := all

# ---- host detection --------------------------------------------------------
GOHOSTOS := $(shell go env GOHOSTOS)
TARGET_OS ?= $(GOHOSTOS)

ifneq ($(TARGET_OS),$(GOHOSTOS))
  export GOOS := $(TARGET_OS)
  export GOARCH ?= amd64
endif

export CGO_ENABLED := 0

# ---- recipe shell ----------------------------------------------------------
# Make runs recipes through sh.exe when it is on PATH (Git Bash, CI) and
# through cmd.exe otherwise (PowerShell).
ifeq ($(shell echo $$0),$$0)
  SHELL_KIND := cmd
else
  SHELL_KIND := sh
endif

ifeq ($(SHELL_KIND),sh)
  RUN = ./$(BIN)
else
  RUN = $(BIN)
endif

# Output binary. Windows names it .exe when targeting Windows.
ifeq ($(TARGET_OS),windows)
  BIN ?= agentflow.exe
else
  BIN ?= agentflow
endif

# Release stamp. Ordinary builds say "dev"; a tag or dirty checkout is visible
# in `agentflow version` and the web console.
VERSION ?= $(or $(shell git describe --tags --always --dirty),dev)
LDFLAGS := -X main.version=$(VERSION)

# Config used by `make run` (your local instance config; gitignored).
CONFIG ?= $(if $(wildcard agentflow.yaml),agentflow.yaml,config.yaml)

# ---- Go binary -------------------------------------------------------------
GOSRC := $(shell git ls-files -co --exclude-standard -- "*.go") \
	$(shell git ls-files -co --exclude-standard -- "internal/builtins/lua") \
	go.mod go.sum

$(BIN): $(GOSRC)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/agentflow

all: $(BIN)

build: $(BIN)

run: $(BIN)
	$(RUN) -config $(CONFIG)

test:
	go test ./...

vet:
	go vet ./...

# Format/check only files touched by this working tree.
define NL


endef
CHANGED_GO := $(subst $(NL), ,$(shell git diff --name-only HEAD -- "*.go")) \
	$(subst $(NL), ,$(shell git ls-files --others --exclude-standard -- "*.go"))

fmt:
	@$(if $(strip $(CHANGED_GO)),gofmt -w $(CHANGED_GO))

FMT_OUT = $(if $(strip $(CHANGED_GO)),$(shell gofmt -l $(CHANGED_GO)))

fmt-check:
	$(if $(strip $(FMT_OUT)),$(error gofmt needed: $(FMT_OUT)),@echo gofmt clean)

check: vet test

schema: $(BIN)
	$(RUN) config schema > agentflow.schema.json
	@echo "wrote agentflow.schema.json"

loop-check: $(BIN)
	$(RUN) loop check plugin:per_chat
	$(RUN) loop check plugin:token_budget

docker:
	docker build -t agentflow:$(VERSION) .

CLEANBINS := $(sort $(BIN) agentflow agentflow.exe)

ifeq ($(SHELL_KIND),sh)
clean:
	rm -rf build/obj internal/vm/lib $(CLEANBINS)
else
clean:
	if exist "build\obj" rmdir /s /q "build\obj"
	if exist "internal\vm\lib" rmdir /s /q "internal\vm\lib"
	$(foreach b,$(CLEANBINS),if exist "$(b)" del /q "$(b)"&)
endif

.PHONY: all build run test vet fmt fmt-check check schema loop-check docker clean
