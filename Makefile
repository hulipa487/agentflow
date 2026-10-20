# AgentFlow Makefile — builds the vendored Luau static library, then the Go binary.
#
#   make            # luau + agentflow binary (default)
#   make luau       # compile vendored Luau -> internal/vm/lib/libluau.a
#   make build      # just the Go binary (requires libluau.a)
#   make test       # go test with the cgo toolchain
#   make vet        # go vet with the cgo toolchain
#   make run        # build, then run with CONFIG (default: config.yaml)
#   make clean      # remove objects, static lib, and binary
#
# Toolchain is auto-selected by host OS:
#   Windows -> zig (hermetic; no system gcc required)
#   Linux   -> system gcc (cc / c++ / ar)
#   macOS   -> clang++ / ar (Apple toolchain)
# Requires GNU make, Go 1.25+, and (on Windows) zig.

.DEFAULT_GOAL := all

# ---- host detection --------------------------------------------------------
# GOHOSTOS / GOHOSTARCH are set by the Go toolchain from `go env` and reflect
# the build host, not the target. They're stable across `make` invocations.
GOHOSTOS  := $(shell go env GOHOSTOS)
GOHOSTARCH:= $(shell go env GOHOSTARCH)

# ---- toolchain (auto per OS) ------------------------------------------------
# Windows uses zig as a hermetic C/C++ toolchain (zig cc wraps a clang frontend
# and a native target triple). Linux uses the system gcc; macOS uses clang++.
# TARGET is only meaningful for zig (its -target flag); the native toolchains
# build for the host without one.
TARGET_OS ?= $(GOHOSTOS)

ifeq ($(GOHOSTOS),windows)
  ifeq ($(TARGET_OS),linux)
    CC       := zig cc -target x86_64-linux-gnu
    CXX      := zig c++ -target x86_64-linux-gnu
    AR       := zig ar
    CXXFLAGS += -target x86_64-linux-gnu -include stdlib.h
    GOENV    := GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-linux-gnu" CXX="zig c++ -target x86_64-linux-gnu"
  else
    CC       := zig cc
    CXX      := zig c++
    AR       := zig ar
    # zig's -target uses x86_64/aarch64, not Go's amd64/arm64.
    ZIG_ARCH := amd64
    ifeq ($(GOHOSTARCH),amd64)
      ZIG_ARCH := x86_64
    else ifeq ($(GOHOSTARCH),arm64)
      ZIG_ARCH := aarch64
    endif
    CXXFLAGS += -target $(ZIG_ARCH)-windows-gnu -include stdlib.h
    GOENV    := CGO_ENABLED=1 CC="zig cc" CXX="zig c++"
  endif
else ifeq ($(GOHOSTOS),linux)
  CC      := cc
  CXX     := c++
  AR      := ar
  GOENV   := CGO_ENABLED=1 CC="cc" CXX="c++"
else ifeq ($(GOHOSTOS),darwin)
  CC      := clang
  CXX     := clang++
  AR      := ar
  GOENV   := CGO_ENABLED=1 CC="clang" CXX="clang++"
else
  $(error unsupported host OS: $(GOHOSTOS))
endif

# Output binary. On Windows you may want: make BIN=agentflow.exe
BIN     ?= agentflow

# Release stamp. Ordinary builds say "dev"; a tag or dirty checkout is visible
# in `agentflow version` and the web console.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Config used by `make run` (your local instance config; gitignored). Prefer
# the filename the binary defaults to, but keep the historical config.yaml
# working for existing checkouts.
CONFIG  ?= $(if $(wildcard agentflow.yaml),agentflow.yaml,config.yaml)

# ---- Luau static library ---------------------------------------------------
LUAU    := third_party/luau
OBJ     := build/obj
LIB     := internal/vm/lib/libluau.a

INCFLAGS := \
	-I$(LUAU)/VM/include \
	-I$(LUAU)/VM/src \
	-I$(LUAU)/Common/include \
	-I$(LUAU)/Ast/include \
	-I$(LUAU)/Bytecode/include \
	-I$(LUAU)/Compiler/include

CXXFLAGS += -O2 -DNDEBUG -std=c++17

# Objects are tagged per module (vm_, common_, ast_, bc_, cc_) to avoid name
# collisions between same-named files in different Luau modules.
OBJS := \
	$(patsubst $(LUAU)/VM/src/%.cpp,$(OBJ)/vm_%.o,$(wildcard $(LUAU)/VM/src/*.cpp)) \
	$(patsubst $(LUAU)/Common/src/%.cpp,$(OBJ)/common_%.o,$(wildcard $(LUAU)/Common/src/*.cpp)) \
	$(patsubst $(LUAU)/Ast/src/%.cpp,$(OBJ)/ast_%.o,$(wildcard $(LUAU)/Ast/src/*.cpp)) \
	$(patsubst $(LUAU)/Bytecode/src/%.cpp,$(OBJ)/bc_%.o,$(wildcard $(LUAU)/Bytecode/src/*.cpp)) \
	$(patsubst $(LUAU)/Compiler/src/%.cpp,$(OBJ)/cc_%.o,$(wildcard $(LUAU)/Compiler/src/*.cpp))

$(OBJ)/vm_%.o: $(LUAU)/VM/src/%.cpp | $(OBJ)
	$(CXX) $(CXXFLAGS) $(INCFLAGS) -c $< -o $@

$(OBJ)/common_%.o: $(LUAU)/Common/src/%.cpp | $(OBJ)
	$(CXX) $(CXXFLAGS) $(INCFLAGS) -c $< -o $@

$(OBJ)/ast_%.o: $(LUAU)/Ast/src/%.cpp | $(OBJ)
	$(CXX) $(CXXFLAGS) $(INCFLAGS) -c $< -o $@

$(OBJ)/bc_%.o: $(LUAU)/Bytecode/src/%.cpp | $(OBJ)
	$(CXX) $(CXXFLAGS) $(INCFLAGS) -c $< -o $@

$(OBJ)/cc_%.o: $(LUAU)/Compiler/src/%.cpp | $(OBJ)
	$(CXX) $(CXXFLAGS) $(INCFLAGS) -c $< -o $@

$(OBJ):
	mkdir -p $(OBJ)

$(LIB): $(OBJS)
	mkdir -p $(dir $(LIB))
	$(AR) rcs $@ $^

luau: $(LIB)

# ---- Go binary -------------------------------------------------------------
# Prerequisites beyond the static lib: every Go source (the prelude is one),
# the module files, and the cgo shim. Without these a Go-only change reports
# nothing-to-do and hands out the stale binary.
# Embedded assets count as sources: a change to a builtin Lua chunk, the
# prelude or the console SPA is compiled into the binary by go:embed, and
# without these a Lua-only edit reports nothing-to-do and hands out a stale
# binary.
GOSRC := $(shell find cmd internal -name '*.go' 2>/dev/null) \
	$(shell find internal/builtins/lua internal/webui/static -type f 2>/dev/null) \
	internal/vm/shim.cpp internal/vm/shim.h \
	go.mod go.sum

$(BIN): $(LIB) $(GOSRC)
	$(GOENV) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/agentflow

all: $(BIN)

build: $(BIN)

run: $(BIN)
	./$(BIN) -config $(CONFIG)

test:
	$(GOENV) go test ./...

vet:
	$(GOENV) go vet ./...

# Format/check only files touched by this working tree. The repository's
# baseline predates the current gofmt (it has many untouched files the tool
# would rewrite), so a repo-wide -w would bury a real change in formatting
# noise.
CHANGED_GO := $(shell git diff --name-only HEAD -- '*.go'; git ls-files --others --exclude-standard '*.go')

fmt:
	@files="$(CHANGED_GO)"; \
	if [ -n "$$files" ]; then gofmt -w $$files; fi

fmt-check:
	@files="$(CHANGED_GO)"; \
	if [ -z "$$files" ]; then exit 0; fi; \
	out="$$(gofmt -l $$files)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: vet test

schema: $(BIN)
	./$(BIN) config schema > agentflow.schema.json
	@echo "wrote agentflow.schema.json"

loop-check: $(BIN)
	./$(BIN) loop check examples/loops/echo.lua
	./$(BIN) loop test examples/scenarios/echo.yaml

docker:
	docker build -t agentflow:$(VERSION) .

clean:
	rm -rf $(OBJ) $(LIB) $(BIN) $(BIN).exe


.PHONY: all luau build run test vet fmt fmt-check check schema loop-check docker clean
