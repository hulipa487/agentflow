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
    GOOS     := linux
    GOARCH   := amd64
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
  endif
else ifeq ($(GOHOSTOS),linux)
  CC      := cc
  CXX     := c++
  AR      := ar
else ifeq ($(GOHOSTOS),darwin)
  CC      := clang
  CXX     := clang++
  AR      := ar
else
  $(error unsupported host OS: $(GOHOSTOS))
endif

# Exported instead of prefixed onto each go invocation. Under PowerShell make
# runs recipes through cmd.exe, where a POSIX "VAR=value cmd" prefix fails with
# "'CGO_ENABLED' is not recognized as an internal or external command".
export CGO_ENABLED := 1
export CC
export CXX
ifdef GOOS
  export GOOS
  export GOARCH
endif

# ---- recipe shell ----------------------------------------------------------
# Make runs recipes through sh.exe when it is on PATH (Git Bash, CI) and
# through cmd.exe otherwise (PowerShell). Probe with `echo $0`: sh prints its
# own path, cmd echoes the literal "$0". Everything below that differs between
# the two shells is selected from SHELL_KIND.
ifeq ($(shell echo $$0),$$0)
  SHELL_KIND := cmd
else
  SHELL_KIND := sh
endif

ifeq ($(SHELL_KIND),sh)
  # Recursive so BIN (defined below) is expanded when the recipe runs.
  RUN      = ./$(BIN)
  MKDIR    = mkdir -p "$1"
else
  RUN      = $(BIN)
  # cmd's mkdir creates intermediate directories itself and rejects -p.
  MKDIR    = if not exist "$(subst /,\,$1)" mkdir "$(subst /,\,$1)"
endif

# Output binary. Windows names it .exe so the target make tracks is the file Go
# actually writes; cross-compiling a Linux binary from Windows keeps the plain
# name.
ifeq ($(GOHOSTOS),windows)
  ifeq ($(TARGET_OS),linux)
    BIN ?= agentflow
  else
    BIN ?= agentflow.exe
  endif
else
  BIN ?= agentflow
endif

# Release stamp. Ordinary builds say "dev"; a tag or dirty checkout is visible
# in `agentflow version` and the web console. No stderr redirect: "2>/dev/null"
# is an invalid path under cmd.exe and made the version fall back to "dev".
VERSION ?= $(or $(shell git describe --tags --always --dirty),dev)
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
	$(call MKDIR,$@)

$(LIB): $(OBJS)
	$(call MKDIR,$(patsubst %/,%,$(dir $(LIB))))
	$(AR) rcs $@ $^

luau: $(LIB)

# ---- Go binary -------------------------------------------------------------
# Every Go source (tracked or untracked-but-not-ignored), the module files and
# the cgo shim. Without these a Go-only change reports nothing-to-do and hands
# out the stale binary.
# Embedded assets count as sources: a change to a builtin Lua chunk, the
# prelude or the console SPA is compiled into the binary by go:embed, and
# without these a Lua-only edit reports nothing-to-do and hands out a stale
# binary.
# git ls-files rather than find: under PowerShell make's $(shell) runs through
# cmd.exe, where "find" is C:\Windows\System32\find.exe and the Go sources
# silently never make it into this list.
GOSRC := $(shell git ls-files -co --exclude-standard -- "*.go") \
	$(shell git ls-files -co --exclude-standard -- "internal/builtins/lua" "internal/webui/static") \
	internal/vm/shim.cpp internal/vm/shim.h \
	go.mod go.sum

$(BIN): $(LIB) $(GOSRC)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/agentflow

all: $(BIN)

build: $(BIN)

run: $(BIN)
	$(RUN) -config $(CONFIG)

test:
	go test ./...

vet:
	go vet ./...

# Format/check only files touched by this working tree. The repository's
# baseline predates the current gofmt (it has many untouched files the tool
# would rewrite), so a repo-wide -w would bury a real change in formatting
# noise.
# Two separate git calls: cmd.exe has no ";" statement separator, so chaining
# them in one $(shell) broke under PowerShell. NL turns the newline-separated
# output into a space-separated argument list.
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
	$(RUN) loop check examples/loops/echo.lua
	$(RUN) loop test examples/scenarios/echo.yaml

docker:
	docker build -t agentflow:$(VERSION) .

# Both names: a Linux-named binary can be left behind by a cross build or an
# older checkout even when the host default is agentflow.exe.
CLEANBINS := $(sort $(BIN) agentflow agentflow.exe)

ifeq ($(SHELL_KIND),sh)
clean:
	rm -rf $(OBJ) $(LIB) $(CLEANBINS)
else
clean:
	if exist "$(subst /,\,$(OBJ))" rmdir /s /q "$(subst /,\,$(OBJ))"
	if exist "$(subst /,\,$(LIB))" del /q "$(subst /,\,$(LIB))"
	$(foreach b,$(CLEANBINS),if exist "$(b)" del /q "$(b)"&)
endif


.PHONY: all luau build run test vet fmt fmt-check check schema loop-check docker clean
