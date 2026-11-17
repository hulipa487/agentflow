package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
)

// The registry client below speaks just enough of the OCI distribution
// protocol to fetch a public or token-protected image by digest: manifests,
// blobs and the anonymous bearer-token handshake. It is written out rather
// than imported because the whole of it is smaller than the dependency tree of
// the usual client libraries, and an engine that ships as one binary should
// not grow by a registry SDK to download a few tarballs.

const (
	mediaIndexOCI     = "application/vnd.oci.image.index.v1+json"
	mediaIndexDocker  = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaManifestOCI  = "application/vnd.oci.image.manifest.v1+json"
	mediaManifestDock = "application/vnd.docker.distribution.manifest.v2+json"

	maxManifestBytes = 4 << 20
	// maxImageBytes bounds what one image may unpack to. Layers are compressed
	// data from the network; without a bound a small download can fill a disk.
	maxImageBytes = 16 << 30
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ImageRef is a parsed image reference. Digest is always set: an environment
// is a declaration, and a tag is a pointer somebody else can move.
type ImageRef struct {
	Registry string // host[:port]
	Repo     string
	Digest   string // sha256:…
}

// ParseImageRef parses "[registry/]repo[:tag]@sha256:…". The tag is accepted
// and ignored, so a reference copied from a registry page works as it is.
func ParseImageRef(s string) (ImageRef, error) {
	name, digest, ok := strings.Cut(strings.TrimSpace(s), "@")
	if !ok || !digestRE.MatchString(digest) {
		return ImageRef{}, fmt.Errorf("image %q must be pinned by digest (name@sha256:…): a tag can be moved to different content", s)
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	if name == "" || strings.ContainsAny(name, " \t") {
		return ImageRef{}, fmt.Errorf("image %q has no repository name", s)
	}
	ref := ImageRef{Registry: "registry-1.docker.io", Repo: name, Digest: digest}
	// The first segment is a registry when it looks like a host; otherwise the
	// whole name is a Docker Hub repository.
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref.Registry, ref.Repo = first, rest
		if first == "docker.io" || first == "index.docker.io" {
			ref.Registry = "registry-1.docker.io"
		}
	}
	if ref.Registry == "registry-1.docker.io" && !strings.Contains(ref.Repo, "/") {
		ref.Repo = "library/" + ref.Repo
	}
	return ref, nil
}

type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

type ociManifest struct {
	MediaType string          `json:"mediaType"`
	Manifests []ociDescriptor `json:"manifests"` // an index
	Config    ociDescriptor   `json:"config"`    // a manifest
	Layers    []ociDescriptor `json:"layers"`
}

type registry struct {
	client *http.Client
	scheme string // https, or http for a test registry
	ref    ImageRef
	token  string
}

// pullImage downloads an image and unpacks it into root, returning the
// environment variables the image declares. Every byte is checked against the
// digest that named it, so the pinned digest in the config vouches for the
// whole tree.
func pullImage(ctx context.Context, client *http.Client, scheme string, ref ImageRef, root string) ([]string, error) {
	reg := &registry{client: client, scheme: scheme, ref: ref}

	raw, err := reg.manifest(ctx, ref.Digest)
	if err != nil {
		return nil, err
	}
	var man ociManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, fmt.Errorf("image manifest: %w", err)
	}
	if len(man.Manifests) > 0 {
		// A multi-platform image: take the entry for this machine.
		var pick string
		for _, d := range man.Manifests {
			if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == runtime.GOARCH {
				pick = d.Digest
				break
			}
		}
		if pick == "" {
			return nil, fmt.Errorf("image has no linux/%s variant", runtime.GOARCH)
		}
		if raw, err = reg.manifest(ctx, pick); err != nil {
			return nil, err
		}
		man = ociManifest{}
		if err := json.Unmarshal(raw, &man); err != nil {
			return nil, fmt.Errorf("image manifest: %w", err)
		}
	}
	if len(man.Layers) == 0 {
		return nil, errors.New("image manifest lists no layers")
	}

	var env []string
	if man.Config.Digest != "" {
		body, err := reg.blob(ctx, man.Config.Digest)
		if err != nil {
			return nil, err
		}
		cfgRaw, err := readVerified(body, man.Config.Digest, maxManifestBytes)
		if err != nil {
			return nil, fmt.Errorf("image config: %w", err)
		}
		var cfg struct {
			Config struct {
				Env []string `json:"Env"`
			} `json:"config"`
		}
		if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
			return nil, fmt.Errorf("image config: %w", err)
		}
		env = cfg.Config.Env
	}

	budget := int64(maxImageBytes)
	for i, layer := range man.Layers {
		if !digestRE.MatchString(layer.Digest) {
			return nil, fmt.Errorf("layer %d has no sha256 digest", i+1)
		}
		body, err := reg.blob(ctx, layer.Digest)
		if err != nil {
			return nil, err
		}
		sum := sha256.New()
		err = applyLayer(root, io.TeeReader(body, sum), &budget)
		if err == nil {
			// The unpacker stops at the end of the archive; whatever follows
			// still counts towards the digest.
			_, err = io.Copy(io.Discard, io.TeeReader(body, sum))
		}
		body.Close()
		if err != nil {
			return nil, fmt.Errorf("layer %d of %d: %w", i+1, len(man.Layers), err)
		}
		if got := "sha256:" + hex.EncodeToString(sum.Sum(nil)); got != layer.Digest {
			return nil, fmt.Errorf("layer %d of %d does not match its digest (got %s, want %s)", i+1, len(man.Layers), got, layer.Digest)
		}
	}
	return env, nil
}

func (r *registry) url(kind, digest string) string {
	return fmt.Sprintf("%s://%s/v2/%s/%s/%s", r.scheme, r.ref.Registry, r.ref.Repo, kind, digest)
}

// manifest fetches a manifest or index by digest and verifies it.
func (r *registry) manifest(ctx context.Context, digest string) ([]byte, error) {
	accept := strings.Join([]string{mediaIndexOCI, mediaIndexDocker, mediaManifestOCI, mediaManifestDock}, ", ")
	body, err := r.get(ctx, r.url("manifests", digest), accept)
	if err != nil {
		return nil, err
	}
	raw, err := readVerified(body, digest, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("image manifest: %w", err)
	}
	return raw, nil
}

func (r *registry) blob(ctx context.Context, digest string) (io.ReadCloser, error) {
	return r.get(ctx, r.url("blobs", digest), "")
}

// get performs a request, doing the bearer-token handshake once when the
// registry asks for it. Docker Hub and most public registries require a token
// even for anonymous pulls.
func (r *registry) get(ctx context.Context, target, accept string) (io.ReadCloser, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if r.token != "" {
			req.Header.Set("Authorization", "Bearer "+r.token)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", r.ref.Registry, err)
		}
		if resp.StatusCode == http.StatusOK {
			return resp.Body, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return nil, fmt.Errorf("registry %s answered %s for %s", r.ref.Registry, resp.Status, r.ref.Repo)
		}
		if err := r.authorize(ctx, challenge); err != nil {
			return nil, err
		}
	}
}

var challengeRE = regexp.MustCompile(`(\w+)="([^"]*)"`)

// authorize exchanges a Bearer challenge for an anonymous pull token.
func (r *registry) authorize(ctx context.Context, challenge string) error {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return fmt.Errorf("registry %s wants credentials (%q); only public images are supported", r.ref.Registry, challenge)
	}
	params := map[string]string{}
	for _, m := range challengeRE.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" {
		return fmt.Errorf("registry %s sent an unusable token endpoint %q", r.ref.Registry, params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + r.ref.Repo + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token endpoint answered %s; only public images are supported", resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	if r.token = tok.Token; r.token == "" {
		r.token = tok.AccessToken
	}
	if r.token == "" {
		return errors.New("registry token endpoint returned no token")
	}
	return nil
}

// readVerified reads a small document and checks it against its digest.
func readVerified(body io.ReadCloser, digest string, limit int64) ([]byte, error) {
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	sum := sha256.Sum256(raw)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("content does not match its digest (got %s, want %s)", got, digest)
	}
	return raw, nil
}
