# AgentFlow Makefile — builds the vendored Luau static library, then the Go binary.
#
#   make            # luau + agentflow binary (default)
#   make luau       # compile vendored Luau -> internal/vm/lib/libluau.a
#   make build      # just the Go binary (requires libluau.a)
#   make test       # go test with the cgo/zig toolchain
#   make vet        # go vet with the cgo/zig toolchain
#   make run        # build, then run with CONFIG (default: examples/agentflow.minimal.yaml)
#   make clean      # remove objects, static lib, and binary
#
# Requires GNU make, zig (as hermetic C/C++ toolchain), and Go 1.25+.

.DEFAULT_GOAL := all

# ---- toolchain -------------------------------------------------------------
CC      := zig cc
CXX     := zig c++
AR      := zig ar

# Luau cross-compile target. Override: make TARGET=x86_64-linux-gnu
TARGET  ?= x86_64-windows-gnu

# Output binary. On Windows you may want: make BIN=agentflow.exe
BIN     ?= agentflow

# Config used by `make run`
CONFIG  ?= examples/agentflow.minimal.yaml

# cgo environment for all Go commands
GOENV   := CGO_ENABLED=1 CC="zig cc" CXX="zig c++"

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

CXXFLAGS := -target $(TARGET) -O2 -DNDEBUG -std=c++17

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
