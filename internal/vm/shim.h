// shim.h — C ABI between Go and the Luau VM (implemented in shim.cpp).
//
// One afvm owns one lua_State (main) plus one loop thread (coroutine).
// All values cross the boundary as JSON strings; the single Lua-visible
// primitive is `__af_op(request_json)` which YIELDS — Go handles the op
// and resumes with (response_json, ok).
#ifndef AF_SHIM_H
#define AF_SHIM_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct afvm afvm;

// Create a sandboxed state (selective stdlib: no os/io/debug).
// instr_budget: interrupt-callback hits allowed per resume before the
// script is killed (0 = unlimited).
afvm* afvm_new(long instr_budget);
void  afvm_close(afvm* v);

// Compile + execute a chunk (typically to define globals).
// Returns 0 on success, 1 on error (message copied into err).
int   afvm_eval(afvm* v, const char* name, const char* code, size_t len,
                char* err, size_t errlen);

// Seal globals, spawn the loop thread, load `code` ONTO the loop thread and
// run it (top-level code may yield — it is driven exactly like loop code),
// then resume global `fn`.
// Returns AF_FINISHED (0), AF_YIELDED (1), or AF_ERROR (2).
#define AF_FINISHED 0
#define AF_YIELDED  1
#define AF_ERROR    2
int   afvm_start(afvm* v, const char* fn, const char* code, size_t len,
                 char* err, size_t errlen);

// Resume a yielded loop with the op response. ok=0 raises in Lua.
int   afvm_resume(afvm* v, const char* resp, size_t len, int ok);

// Syntax-check only (parse; no state, no execution). 0 = ok.
int   afvm_check(const char* name, const char* code, size_t len,
                 char* err, size_t errlen);

// Length of the current yield-request / error message (bytes, no NUL).
// If buf != NULL, copies up to buflen bytes.
size_t afvm_lastmsg(afvm* v, char* buf, size_t buflen);

// Reset the instruction budget (e.g. between turns).
void  afvm_set_budget(afvm* v, long instr_budget);

#ifdef __cplusplus
}
#endif

#endif // AF_SHIM_H
