#!/usr/bin/env bash
# build-luau.sh — compile vendored Luau (VM + Ast + Compiler) into a static
# library for cgo, using zig c++ as a hermetic toolchain.
#
# Usage:
#   build/build-luau.sh                 # native (windows/amd64 host)
#   AF_TARGET=x86_64-linux-gnu build/build-luau.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LUAU="$ROOT/third_party/luau"
OBJ="$ROOT/build/obj"
OUT="$ROOT/internal/vm/lib"
mkdir -p "$OBJ" "$OUT"

CXX="${AF_CXX:-zig} c++"
TARGET="${AF_TARGET:-x86_64-windows-gnu}"

INC=(
  -I"$LUAU/VM/include"
  -I"$LUAU/VM/src"
  -I"$LUAU/Common/include"
  -I"$LUAU/Ast/include"
  -I"$LUAU/Bytecode/include"
  -I"$LUAU/Compiler/include"
)
FLAGS=(
  -target "$TARGET"
  -O2
  -DNDEBUG
  -std=c++17
)

compile_dir() {
  local dir="$1" tag="$2" f o
  for f in "$dir"/*.cpp; do
    o="$OBJ/${tag}_$(basename "${f%.cpp}").o"
    if [[ ! -f "$o" || "$f" -nt "$o" ]]; then
      echo "  c++ ${tag}/$(basename "$f")"
      $CXX "${FLAGS[@]}" "${INC[@]}" -c "$f" -o "$o"
    fi
  done
}

echo "== compiling Luau VM =="
compile_dir "$LUAU/VM/src" vm
echo "== compiling Luau Common =="
compile_dir "$LUAU/Common/src" common
echo "== compiling Luau Ast =="
compile_dir "$LUAU/Ast/src" ast
echo "== compiling Luau Bytecode =="
compile_dir "$LUAU/Bytecode/src" bc
echo "== compiling Luau Compiler =="
compile_dir "$LUAU/Compiler/src" cc

echo "== archiving =="
zig ar rcs "$OUT/libluau.a" "$OBJ"/*.o
echo "built: $OUT/libluau.a"
