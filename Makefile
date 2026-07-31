# AgentFlow Makefile — builds the vendored Luau static library, then the Go binary.
#
#   make            # luau + agentflow binary (default)
#   make luau       # compile vendored Luau -> internal/vm/lib/libluau.a
#   make build      # just the Go binary (requires libluau.a)
#   make test       # go test with the cgo toolchain
#   make vet        # go vet with the cgo toolchain
#   make run        # build, then run with CONFIG (default: examples/agentflow.minimal.yaml)
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
ifeq ($(GOHOSTOS),windows)
  CC      := zig cc
  CXX     := zig c++
  AR      := zig ar
  # zig's -target uses x86_64/aarch64, not Go's amd64/arm64.
  ZIG_ARCH := amd64
  ifeq ($(GOHOSTARCH),amd64)
    ZIG_ARCH := x86_64
  else ifeq ($(GOHOSTARCH),arm64)
    ZIG_ARCH := aarch64
  endif
  CXXFLAGS += -target $(ZIG_ARCH)-windows-gnu
  GOENV   := CGO_ENABLED=1 CC="zig cc" CXX="zig c++"
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

# Config used by `make run`
CONFIG  ?= examples/agentflow.minimal.yaml

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
$(BIN): $(LIB)
	$(GOENV) go build -o $(BIN) ./cmd/agentflow

all: $(BIN)

build: $(BIN)

run: $(BIN)
	./$(BIN) -config $(CONFIG)

test:
	$(GOENV) go test ./...

vet:
	$(GOENV) go vet ./...

clean:
	rm -rf $(OBJ) $(LIB) $(BIN) $(BIN).exe


.PHONY: all luau build run test vet clean
