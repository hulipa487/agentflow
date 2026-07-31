// shim.cpp — Luau embedding behind a small C ABI (see shim.h).
//
// Sandboxing model (phase 0):
//   - selective stdlib: base/coroutine/table/string/math/bit32/utf8/buffer
//     (no os, no io, no debug)
//   - luaL_sandbox on the main state after user code loads (seals globals),
//     luaL_sandboxthread on the loop thread (safe inherited environment)
//   - instruction budget via the Luau interrupt callback
//
// Async bridge: the Lua-visible `__af_op(request_json)` C function yields;
// the host (Go) handles the op and resumes with (response_json, ok).
#include "shim.h"

#include <string.h>
#include <stdlib.h>
#include <string>
#include <string_view>

#include "lua.h"
#include "lualib.h"
#include "Luau/Compiler.h"
#include "Luau/Parser.h"

struct afvm {
    lua_State* L;   // main state
    lua_State* T;   // loop thread (coroutine)
    long budget;
    long budget_max;
    int  phase;     // 0 = running plugin chunk, 1 = running loop fn
    char fn[64];    // loop function name
};

// ---------------------------------------------------------------- budget ---

static afvm* vm_from(lua_State* L) {
    lua_getfield(L, LUA_REGISTRYINDEX, "afvm");
    afvm* v = (afvm*)lua_touserdata(L, -1);
    lua_pop(L, 1);
    return v;
}

// Called by Luau at safepoints (calls, loop back-edges). Throwing here is
// the supported way to kill a runaway script.
static void af_interrupt(lua_State* L, int /*gc*/) {
    afvm* v = vm_from(L);
    if (!v || v->budget_max == 0) return;
    if (--v->budget <= 0) {
        v->budget = v->budget_max; // re-arm so the error itself can't loop
        luaL_error(L, "instruction budget exceeded");
    }
}

// -------------------------------------------------------------- af bridge ---

// Lua-visible: __af_op(request_json) -> (response_json, ok)
// Yields the request; Go resumes with the two return values.
static int af_op(lua_State* L) {
    luaL_checkstring(L, 1);
    lua_settop(L, 1);
    return lua_yield(L, 1);
}

// ------------------------------------------------------------------ libs ---

static void open_libs(lua_State* L) {
    static const luaL_Reg libs[] = {
        {"", luaopen_base},
        {"coroutine", luaopen_coroutine},
        {"table", luaopen_table},
        {"string", luaopen_string},
        {"math", luaopen_math},
        {"bit32", luaopen_bit32},
        {"utf8", luaopen_utf8},
        {"buffer", luaopen_buffer},
        {NULL, NULL},
    };
    for (const luaL_Reg* lib = libs; lib->func; ++lib) {
        lua_pushcfunction(L, lib->func, lib->name);
        lua_call(L, 0, 0);
    }
}

// ------------------------------------------------------------------- api ---

afvm* afvm_new(long instr_budget) {
    afvm* v = (afvm*)calloc(1, sizeof(afvm));
    v->L = luaL_newstate();
    v->budget = v->budget_max = instr_budget;

    open_libs(v->L);

    // registry back-pointer for the interrupt callback
    lua_pushlightuserdata(v->L, v);
    lua_setfield(v->L, LUA_REGISTRYINDEX, "afvm");
    lua_callbacks(v->L)->interrupt = af_interrupt;

    lua_pushcfunction(v->L, af_op, "__af_op");
    lua_setglobal(v->L, "__af_op");
    return v;
}

void afvm_close(afvm* v) {
    if (!v) return;
    if (v->L) lua_close(v->L);
    free(v);
}

static int copy_err(lua_State* L, char* err, size_t errlen) {
    const char* msg = lua_tostring(L, -1);
    if (!msg) msg = "(non-string error)";
    snprintf(err, errlen, "%s", msg);
    lua_pop(L, 1);
    return 1;
}

int afvm_eval(afvm* v, const char* name, const char* code, size_t len,
              char* err, size_t errlen) {
    std::string bc = Luau::compile(std::string(code, len));
    int load = luau_load(v->L, name, bc.data(), bc.size(), 0);
    if (load != 0) return copy_err(v->L, err, errlen);
    if (lua_pcall(v->L, 0, 0, 0) != 0) return copy_err(v->L, err, errlen);
    return 0;
}

static int do_resume(afvm* v, int nargs) {
    v->budget = v->budget_max; // per-resume budget
    int status = lua_resume(v->T, v->L, nargs);
    if (status == LUA_YIELD) return AF_YIELDED;
    if (status == 0) return AF_FINISHED;
    return AF_ERROR;
}

// drive runs the thread; when the plugin chunk finishes (from start OR from
// a later resume), it chains into the loop function transparently.
static int drive(afvm* v, int nargs) {
    int st = do_resume(v, nargs);
    if (st == AF_FINISHED && v->phase == 0) {
        v->phase = 1;
        lua_getfield(v->T, LUA_GLOBALSINDEX, v->fn);
        if (!lua_isfunction(v->T, -1)) {
            lua_pop(v->T, 1);
            lua_pushfstring(v->T, "global '%s' is not defined (loop plugin must define it)", v->fn);
            return AF_ERROR;
        }
        st = do_resume(v, 0);
    }
    return st;
}

int afvm_start(afvm* v, const char* fn, const char* code, size_t len,
               char* err, size_t errlen) {
    // seal the main globals (prelude is fully loaded by now); the loop
    // thread gets a safe environment that inherits them read-only
    luaL_sandbox(v->L);

    v->T = lua_newthread(v->L);
    luaL_sandboxthread(v->T);

    snprintf(v->fn, sizeof(v->fn), "%s", fn);
    v->phase = 0;

    // load the user chunk onto the loop thread: top-level code runs under
    // the same yield/resume driving as the loop itself
    std::string bc = Luau::compile(std::string(code, len));
    if (luau_load(v->T, "@plugin", bc.data(), bc.size(), 0) != 0) {
        copy_err(v->T, err, errlen);
        return AF_ERROR;
    }

    int st = drive(v, 0);
    if (st == AF_ERROR) {
        copy_err(v->T, err, errlen);
        return AF_ERROR;
    }
    return st;
}

int afvm_resume(afvm* v, const char* resp, size_t len, int ok) {
    lua_pushlstring(v->T, resp ? resp : "", resp ? len : 0);
    lua_pushboolean(v->T, ok);
    return drive(v, 2);
}

size_t afvm_lastmsg(afvm* v, char* buf, size_t buflen) {
    size_t len = 0;
    const char* s = lua_tolstring(v->T, -1, &len);
    if (!s) return 0;
    if (buf) {
        size_t n = len < buflen ? len : buflen;
        memcpy(buf, s, n);
    }
    return len;
}

void afvm_set_budget(afvm* v, long instr_budget) {
    v->budget = v->budget_max = instr_budget;
}

int afvm_check(const char* name, const char* code, size_t len,
               char* err, size_t errlen) {
    Luau::Allocator alloc;
    Luau::AstNameTable names(alloc);
    Luau::ParseResult res = Luau::Parser::parse(code, len, names, alloc);
    if (!res.errors.empty()) {
        const Luau::ParseError& e = res.errors.front();
        snprintf(err, errlen, "%s:%d:%d: %s", name,
                 e.getLocation().begin.line + 1, e.getLocation().begin.column + 1,
                 e.what());
        return 1;
    }
    return 0;
}
