// Secret-field registry and lazy credential resolution.
//
// The legacy single-file path byte-expands ${VAR} over the whole document at
// load, so every secret must exist in the process env and a missing one
// silently becomes "". The configdir path instead loads structured: secret
// fields (the registry below) keep their raw reference — a literal, ${VAR},
// or cred:<service> — and each consumer resolves it at construction:
// process env -> credential store (service = the VAR name, or the name after
// cred:) -> unresolvable. An unresolvable credential on an optional
// component (search engine, channel, memory backend, media store) skips that
// component with a warning; a model api_key is not optional — the model stays
// configured and the first LLM call fails with a clear error.
package config

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"strings"

	"agentflow/internal/core/credentials"
)

// secretPathPatterns enumerates the config locations that carry secrets. A
// path segment "*" matches any single map key; "[]" matches a slice element.
// Anything NOT listed here is environment-expanded at load, as today.
var secretPathPatterns = []string{
	"models.*.api_key",
	"search.engines.*.api_key",
	"gateway.channels.[].token",
	"gateway.channels.[].secret",
	"memory.backends.*.config.url",
	"memory.backends.*.config.password",
	"media.s3.access_key",
	"media.s3.secret_key",
	"profiles.shell.*.password",
}

// expandDeferredSecrets environment-expands every non-secret string in the
// merged config, leaving registry fields raw for lazy resolution. Used by the
// configdir path; the single-file path keeps whole-document byte expansion.
func expandDeferredSecrets(c *Config) {
	walkExpand(reflect.ValueOf(c), nil)
}

func walkExpand(v reflect.Value, path []string) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			walkExpand(v.Field(i), append(path, name))
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		// Map values are unaddressable: copy each into a settable cell,
		// expand it, and write back into a fresh map.
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			k := iter.Key()
			ev := iter.Value()
			if k.Kind() == reflect.String {
				cp := reflect.New(ev.Type()).Elem()
				cp.Set(ev)
				walkExpand(cp, append(path, k.String()))
				ev = cp
			}
			out.SetMapIndex(k, ev)
		}
		v.Set(out)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkExpand(v.Index(i), append(path, "[]"))
		}
	case reflect.String:
		if isSecretPath(path) {
			return // raw reference, resolved lazily at the consumer
		}
		s := v.String()
		if !strings.Contains(s, "${") {
			return
		}
		v.SetString(expandEnvString(s))
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		// The concrete value inside an interface is read-only; expand a
		// settable copy and write it back.
		ev := v.Elem()
		cp := reflect.New(ev.Type()).Elem()
		cp.Set(ev)
		walkExpand(cp, path)
		v.Set(cp)
	}
}

func isSecretPath(path []string) bool {
	for _, pat := range secretPathPatterns {
		segs := strings.Split(pat, ".")
		if len(segs) != len(path) {
			continue
		}
		match := true
		for i, seg := range segs {
			if seg != "*" && seg != path[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// expandEnvString expands ${VAR} and ${VAR:-default} in one string, matching
// the whole-document byte expansion used by the single-file path.
func expandEnvString(s string) string {
	return string(envRe.ReplaceAllFunc([]byte(s), func(m []byte) []byte {
		parts := envRe.FindSubmatch(m)
		if v, ok := os.LookupEnv(string(parts[1])); ok {
			return []byte(v)
		}
		if parts[2] != nil {
			return parts[2]
		}
		return nil
	}))
}

// fullEnvRe matches a string that is EXACTLY one ${VAR} or ${VAR:-default}
// reference (whole-string), as opposed to envRe which matches anywhere.
var fullEnvRe = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}$`)

// SecretRef is a parsed secret config value: a literal, a ${VAR} / ${VAR:-default}
// environment reference, or a cred:<service> store reference.
type SecretRef struct {
	Raw        string
	Literal    string // set when the value is a plain string
	Env        string // VAR name for ${VAR} / ${VAR:-default}
	EnvDefault string
	HasDefault bool
	Cred       string // service name for cred:<service>
}

// ParseSecretRef classifies a raw config value. Only a string that is wholly
// one reference counts as a reference; anything else is a literal.
func ParseSecretRef(s string) SecretRef {
	if rest, ok := strings.CutPrefix(strings.TrimSpace(s), "cred:"); ok {
		return SecretRef{Raw: s, Cred: strings.TrimSpace(rest)}
	}
	if m := fullEnvRe.FindStringSubmatch(s); m != nil {
		ref := SecretRef{Raw: s, Env: m[1]}
		if m[2] != "" || strings.Contains(s, ":-") {
			ref.EnvDefault = m[2]
			ref.HasDefault = true
		}
		return ref
	}
	return SecretRef{Raw: s, Literal: s}
}

// CredentialName names the credential a raw value refers to, for warning logs
// that must identify the missing secret without revealing it.
func CredentialName(raw string) string {
	ref := ParseSecretRef(raw)
	switch {
	case ref.Cred != "":
		return "cred:" + ref.Cred
	case ref.Env != "":
		return ref.Env
	default:
		return raw
	}
}

// OpaqueMarker renders a raw secret value as the opaque marker used on the Lua
// config surface: {env="VAR"} or {cred="service"} for references, the literal
// otherwise. It never resolves — a resolved secret never reaches Lua.
func OpaqueMarker(raw string) any {
	ref := ParseSecretRef(raw)
	switch {
	case ref.Cred != "":
		return map[string]any{"cred": ref.Cred}
	case ref.Env != "":
		return map[string]any{"env": ref.Env}
	default:
		return raw
	}
}

// Resolver resolves lazy secret references at consumer construction. The
// order is process env, then the credential store (engine-wide, under the
// empty user UUID), then unresolvable. A nil/absent store makes cred:
// references (and env fallbacks) unresolvable, which feeds the skip/error
// policy at the consumer.
type Resolver struct {
	Store *credentials.Store
}

// Resolve resolves one raw config value. Literals (including values the
// legacy path already expanded at load) pass through. ${VAR:-default} uses
// the default when the env var is unset, never the store.
func (r *Resolver) Resolve(ctx context.Context, raw string) (string, bool) {
	ref := ParseSecretRef(raw)
	switch {
	case ref.Cred != "":
		if r == nil || r.Store == nil {
			return "", false
		}
		sec, ok, err := r.Store.Get(ctx, "", ref.Cred)
		if err != nil || !ok {
			return "", false
		}
		return sec.Value, true
	case ref.Env != "":
		if v, ok := os.LookupEnv(ref.Env); ok {
			return v, true
		}
		if r != nil && r.Store != nil {
			if sec, ok, err := r.Store.Get(ctx, "", ref.Env); err == nil && ok {
				return sec.Value, true
			}
		}
		if ref.HasDefault {
			return ref.EnvDefault, true
		}
		return "", false
	default:
		return raw, true
	}
}
