package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func file(name, body string) entry {
	return entry{name: name, body: body, typ: tar.TypeReg, mode: 0o644}
}
func dir(name string) entry { return entry{name: name, typ: tar.TypeDir, mode: 0o755} }
func symlink(name, to string) entry {
	return entry{name: name, link: to, typ: tar.TypeSymlink, mode: 0o777}
}

func layerOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func apply(t *testing.T, root string, layer []byte) error {
	t.Helper()
	budget := int64(1 << 30)
	return applyLayer(root, bytes.NewReader(layer), &budget)
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLayersStackWithWhiteouts(t *testing.T) {
	root := t.TempDir()
	if err := apply(t, root, layerOf(t,
		dir("etc/"), file("etc/keep", "1"), file("etc/gone", "1"),
		dir("var/cache/"), file("var/cache/a", "1"), file("var/cache/b", "1"),
		entry{name: "bin/tool", body: "#!/bin/sh\n", typ: tar.TypeReg, mode: 0o4755},
		entry{name: "locked/", typ: tar.TypeDir, mode: 0o555}, file("locked/inside", "1"),
	)); err != nil {
		t.Fatal(err)
	}
	if err := apply(t, root, layerOf(t,
		file("etc/.wh.gone", ""),
		file("var/cache/.wh..wh..opq", ""), file("var/cache/c", "2"),
		file("etc/keep", "2"),
		entry{name: "dev/null", typ: tar.TypeChar, mode: 0o666},
	)); err != nil {
		t.Fatal(err)
	}

	if got := read(t, filepath.Join(root, "etc/keep")); got != "2" {
		t.Errorf("etc/keep = %q, want the upper layer's content", got)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/gone")); !os.IsNotExist(err) {
		t.Error("a whiteout did not delete its file")
	}
	names, _ := os.ReadDir(filepath.Join(root, "var/cache"))
	if len(names) != 1 || names[0].Name() != "c" {
		t.Errorf("an opaque directory kept lower-layer entries: %v", names)
	}
	if _, err := os.Lstat(filepath.Join(root, "dev/null")); !os.IsNotExist(err) {
		t.Error("a device node was created")
	}
	st, err := os.Stat(filepath.Join(root, "bin/tool"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows keeps no Unix modes, so there is nothing to check there.
	if runtime.GOOS != "windows" {
		if st.Mode()&os.ModeSetuid != 0 || st.Mode().Perm() != 0o755 {
			t.Errorf("bin/tool mode = %v, want 0755 without setuid", st.Mode())
		}
		if st, _ := os.Stat(filepath.Join(root, "locked")); st.Mode().Perm()&0o700 != 0o700 {
			t.Errorf("a read-only directory was left unwritable (%v): the tree could not be removed", st.Mode())
		}
	}
}

func TestLayerEntriesNeverLeaveTheRoot(t *testing.T) {
	outside := t.TempDir()
	root := filepath.Join(outside, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	err := apply(t, root, layerOf(t,
		symlink("abs", "/etc"), file("abs/passwd", "inside"),
		symlink("up", "../../.."), file("up/escaped", "inside"),
		file("../../dotdot", "inside"),
		file("/rooted", "inside"),
		dir("d/"), entry{name: "d/hard", link: "../../../abs/passwd", typ: tar.TypeLink},
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"etc/passwd", "escaped", "dotdot", "rooted", "d/hard"} {
		if got := read(t, filepath.Join(root, want)); got != "inside" {
			t.Errorf("%s = %q", want, got)
		}
	}
	left, _ := os.ReadDir(outside)
	if len(left) != 1 {
		t.Errorf("entries were written outside the root: %v", left)
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	for p, want := range map[string]bool{
		root:                                     true,
		filepath.Join(root, "a", "b"):            true,
		filepath.Join(root, "..a"):               true, // a name that starts with dots is still inside
		filepath.Dir(root):                       false,
		filepath.Join(root, "..", "x"):           false,
		filepath.Join(filepath.Dir(root), "roo"): false,
	} {
		if got := within(root, p); got != want {
			t.Errorf("within(root, %q) = %v, want %v", p, got, want)
		}
	}
}

func TestLayerLinkLoopsAndSizeAreBounded(t *testing.T) {
	root := t.TempDir()
	err := apply(t, root, layerOf(t, symlink("a", "b"), symlink("b", "a"), file("a/x", "1")))
	if err == nil || !strings.Contains(err.Error(), "too many symbolic links") {
		t.Errorf("a link loop gave %v", err)
	}
	budget := int64(10)
	err = applyLayer(t.TempDir(), bytes.NewReader(layerOf(t, file("big", strings.Repeat("x", 11)))), &budget)
	if !errors.Is(err, errLayerTooBig) {
		t.Errorf("an oversized layer gave %v", err)
	}
	err = applyLayer(t.TempDir(), bytes.NewReader([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0}), &budget)
	if err == nil || !strings.Contains(err.Error(), "zstd") {
		t.Errorf("a zstd layer gave %v", err)
	}
}

func TestParseImageRef(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	cases := map[string]ImageRef{
		"alpine@" + d:                              {"registry-1.docker.io", "library/alpine", d},
		"python:3.12-slim@" + d:                    {"registry-1.docker.io", "library/python", d},
		"docker.io/library/python:3.12@" + d:       {"registry-1.docker.io", "library/python", d},
		"acme/tools@" + d:                          {"registry-1.docker.io", "acme/tools", d},
		"ghcr.io/acme/tools:v1@" + d:               {"ghcr.io", "acme/tools", d},
		"localhost:5000/dev/img@" + d:              {"localhost:5000", "dev/img", d},
		"registry.example.com:8443/a/b/c:tag@" + d: {"registry.example.com:8443", "a/b/c", d},
	}
	for in, want := range cases {
		got, err := ParseImageRef(in)
		if err != nil || got != want {
			t.Errorf("ParseImageRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"python:3.12", "python@sha256:abc", "@" + d, ""} {
		if _, err := ParseImageRef(bad); err == nil {
			t.Errorf("ParseImageRef(%q) was accepted", bad)
		}
	}
}

// fakeRegistry serves one two-layer image behind the anonymous token
// handshake, as Docker Hub does.
type fakeRegistry struct {
	*httptest.Server
	blobs    map[string][]byte
	index    string
	pulls    atomic.Int32
	tokens   atomic.Int32
	tampered string // a digest whose content is served altered
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{blobs: map[string][]byte{}}
	put := func(b []byte) ociDescriptor {
		d := digestOf(b)
		f.blobs[d] = b
		return ociDescriptor{Digest: d, Size: int64(len(b))}
	}
	l1 := put(layerOf(t, dir("bin/"), entry{name: "bin/sh", body: "shell", typ: tar.TypeReg, mode: 0o755}, dir("root/"), file("etc/old", "1")))
	l2 := put(layerOf(t, file("etc/.wh.old", ""), file("etc/new", "2")))
	cfg := put([]byte(`{"config":{"Env":["PATH=/img/bin","LANG=C.UTF-8"]}}`))
	man, _ := json.Marshal(ociManifest{MediaType: mediaManifestOCI, Config: cfg, Layers: []ociDescriptor{l1, l2}})
	manDesc := put(man)
	other := put([]byte(`{"layers":[]}`))
	platform := func(arch string) *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} {
		return &struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		}{"linux", arch}
	}
	otherArch := "amd64"
	if runtime.GOARCH == "amd64" {
		otherArch = "arm64"
	}
	other.Platform, manDesc.Platform = platform(otherArch), platform(runtime.GOARCH)
	idx, _ := json.Marshal(ociManifest{MediaType: mediaIndexOCI, Manifests: []ociDescriptor{other, manDesc}})
	f.index = put(idx).Digest

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokens.Add(1)
		if r.URL.Query().Get("scope") != "repository:team/img:pull" || r.URL.Query().Get("service") != "fake" {
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"token":"letmein"}`)
	})
	mux.HandleFunc("/v2/team/img/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer letmein" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:team/img:pull"`, f.URL))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		digest := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, ok := f.blobs[digest]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.pulls.Add(1)
		if digest == f.tampered {
			body = append([]byte{}, body...)
			body[len(body)-1] ^= 0xff
		}
		_, _ = w.Write(body)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRegistry) envs(t *testing.T, dir string) *Envs {
	t.Helper()
	image := strings.TrimPrefix(f.URL, "http://") + "/team/img:latest@" + f.index
	e := NewEnvs(dir, map[string]EnvSpec{
		"tools": {Image: image, Env: map[string]string{"LANG": "en_US.UTF-8", "EXTRA": "1"}},
		"local": {Rootfs: t.TempDir()},
	}, nil)
	e.scheme = "http"
	return e
}

func TestEnvironmentPullsItsImageOnce(t *testing.T) {
	reg := newFakeRegistry(t)
	dir := t.TempDir()
	envs := reg.envs(t, dir)

	env, err := envs.Resolve(context.Background(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(env.Rootfs, "bin/sh")); got != "shell" {
		t.Errorf("bin/sh = %q", got)
	}
	if got := read(t, filepath.Join(env.Rootfs, "etc/new")); got != "2" {
		t.Errorf("etc/new = %q", got)
	}
	if _, err := os.Stat(filepath.Join(env.Rootfs, "etc/old")); !os.IsNotExist(err) {
		t.Error("the second layer's whiteout was not applied")
	}
	want := map[string]string{"PATH": "/img/bin", "LANG": "en_US.UTF-8", "EXTRA": "1"}
	if fmt.Sprint(env.Env) != fmt.Sprint(want) {
		t.Errorf("env = %v, want the image's variables with the config's on top: %v", env.Env, want)
	}

	// A second use, and a second process over the same directory, download
	// nothing.
	before := reg.pulls.Load()
	if _, err := envs.Resolve(context.Background(), "tools"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.envs(t, dir).Resolve(context.Background(), "tools"); err != nil {
		t.Fatal(err)
	}
	if reg.pulls.Load() != before {
		t.Errorf("a cached image was downloaded again (%d requests)", reg.pulls.Load()-before)
	}
	if reg.tokens.Load() != 1 {
		t.Errorf("the token handshake ran %d times for one pull", reg.tokens.Load())
	}
}

func TestEnvironmentRefusesContentThatDoesNotMatchItsDigest(t *testing.T) {
	for _, what := range []string{"index", "layer"} {
		t.Run(what, func(t *testing.T) {
			reg := newFakeRegistry(t)
			reg.tampered = reg.index
			if what == "layer" {
				for d, b := range reg.blobs {
					if bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
						reg.tampered = d
						break
					}
				}
			}
			dir := t.TempDir()
			_, err := reg.envs(t, dir).Resolve(context.Background(), "tools")
			if err == nil || !strings.Contains(err.Error(), "digest") || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Resolve = %v, want an unavailable error about the digest", err)
			}
			left, _ := filepath.Glob(filepath.Join(dir, "images", "*"))
			if len(left) != 0 {
				t.Errorf("a refused image was left on disk: %v", left)
			}
		})
	}
}

func TestEnvironmentLocalRootfsAndUnknownNames(t *testing.T) {
	reg := newFakeRegistry(t)
	envs := reg.envs(t, t.TempDir())
	if env, err := envs.Resolve(context.Background(), "local"); err != nil || env.Rootfs == "" {
		t.Fatalf("a local root filesystem did not resolve: %+v, %v", env, err)
	}
	_, err := envs.Resolve(context.Background(), "nope")
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "local, tools") {
		t.Errorf("unknown environment gave %v, want the declared names", err)
	}
	if got := envs.Names(); strings.Join(got, ",") != "local,tools" || !envs.Has("tools") || envs.Has("nope") {
		t.Errorf("Names = %v", got)
	}
}
