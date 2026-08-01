package shell

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

// VultrProvider implements a PERSISTENT shell on a Vultr VPS launched on
// demand. Spawn creates an instance through the Vultr API, waits for it to
// boot, and opens an SSH connection that Exec/Read/Write reuse for the life
// of the handle. Destroy closes the SSH connection and deletes the Vultr
// instance (and any ephemeral SSH key it created).
//
// The Vultr API key is read from the VULTR_API_KEY environment variable at
// construction; if it is unset the provider is disabled and Spawn fails fast.
// The key is never logged.
type VultrProvider struct {
	log    *slog.Logger
	apiKey string // from VULTR_API_KEY; empty = disabled
}

// NewVultrProvider creates the Vultr shell provider. apiKey is the resolved
// VULTR_API_KEY value (caller reads the env var so the failure surfaces at
// boot rather than at first spawn).
func NewVultrProvider(log *slog.Logger, apiKey string) *VultrProvider {
	return &VultrProvider{log: log.With("provider", "vultr"), apiKey: apiKey}
}

func (p *VultrProvider) Name() string { return "vultr" }

// vultrHandleMeta is the teardown record stashed on Handle.Meta.
type vultrHandleMeta struct {
	instanceID string
	sshKeyID  string // empty if we did not create the key
	createdKey bool
	mainIP    string
	user      string
}

// defaultUbuntuOsID is Vultr's OS id for Ubuntu 22.04 x64, used when the caller
// does not supply an os_id. (Resolved from GET /v2/os in the spec; stable for
// the common Ubuntu releases.)
const defaultUbuntuOsID = 1743 // Ubuntu 22.04 x64

func (p *VultrProvider) Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("vultr: VULTR_API_KEY not set")
	}

	region := stringOpt(opts.ShellOpts, "region")
	plan := stringOpt(opts.ShellOpts, "plan")
	if region == "" || plan == "" {
		return nil, fmt.Errorf("vultr: region and plan are required")
	}
	osID := intOpt(opts.ShellOpts, "os_id", defaultUbuntuOsID)
	label := firstNonEmpty(stringOpt(opts.ShellOpts, "label"), "af-"+uuid.New().String()[:8])
	hostname := stringOpt(opts.ShellOpts, "hostname")
	if hostname == "" {
		hostname = label
	}
	tags := stringSliceOpt(opts.ShellOpts, "tags")
	if len(tags) == 0 {
		tags = []string{"agentflow"}
	}

	client := newVultrClient(p.apiKey, stringOpt(opts.ShellOpts, "base_url"))

	// Resolve an SSH key: either a caller-supplied sshkey_id (with a matching
	// key_file private key for the dial), an inline public key to register, or
	// an ephemeral keypair we generate and register.
	signer, sshKeyID, createdKey, err := p.resolveSSHKey(ctx, client, opts)
	if err != nil {
		return nil, err
	}

	// Create the instance.
	instID, err := client.CreateInstance(ctx, instanceCreateReq{
		Region:   region,
		Plan:     plan,
		OsID:     osID,
		Label:    label,
		Hostname: hostname,
		SSHKeyID: []string{sshKeyID},
		Tags:     tags,
	})
	if err != nil {
		if createdKey {
			_ = client.DeleteSSHKey(ctx, sshKeyID)
		}
		return nil, fmt.Errorf("vultr create instance: %w", err)
	}
	p.log.Info("vultr instance creating", "instance_id", instID, "label", label, "region", region, "plan", plan)

	// Wait for the instance to boot and get a public IP.
	inst, err := p.waitForReady(ctx, client, instID)
	if err != nil {
		// Best-effort cleanup of the half-created instance.
		_ = client.DeleteInstance(ctx, instID)
		if createdKey {
			_ = client.DeleteSSHKey(ctx, sshKeyID)
		}
		return nil, err
	}

	user := strings.TrimSpace(inst.UserScheme)
	if user == "" {
		user = "root"
	}

	// Dial SSH (with a short retry window while sshd comes up).
	sshClient, err := sshDialWithSigner(ctx, inst.MainIP+":22", user, signer)
	if err != nil {
		_ = client.DeleteInstance(ctx, instID)
		if createdKey {
			_ = client.DeleteSSHKey(ctx, sshKeyID)
		}
		return nil, fmt.Errorf("vultr ssh dial %s: %w", inst.MainIP, err)
	}

	h := &Handle{
		ID:       "vultr-" + uuid.New().String()[:8],
		Provider: "vultr",
		State:    HandleRunning,
		Image:    "",
		Meta: map[string]any{
			"instance_id": instID,
			"ssh_key_id":  sshKeyID,
			"created_key": createdKey,
			"main_ip":     inst.MainIP,
			"user":        user,
			"label":       label,
		},
		internal: sshClient,
	}
	p.log.Info("vultr shell ready", "handle_id", h.ID, "instance_id", instID, "ip", inst.MainIP)
	return h, nil
}

// resolveSSHKey returns the SSH signer for dialing, the Vultr sshkey_id to
// attach to the instance, and whether we created the key (so Destroy deletes
// it). If the caller supplied a bare sshkey_id plus a key_file, that key is
// reused (no registration). Otherwise an inline public key is registered, or
// — if none is given — an ephemeral ed25519 keypair is generated and its
// public half registered.
func (p *VultrProvider) resolveSSHKey(ctx context.Context, c *vultrClient, opts SpawnOpts) (signer ssh.Signer, sshKeyID string, createdKey bool, err error) {
	bareID := stringOpt(opts.ShellOpts, "sshkey_id")
	pubInline := stringOpt(opts.ShellOpts, "ssh_key")

	switch {
	case bareID != "" && opts.KeyFile != "":
		// Reuse a pre-registered key; dial with its private half from key_file.
		s, perr := loadSignerFromFile(opts.KeyFile)
		if perr != nil {
			return nil, "", false, perr
		}
		return s, bareID, false, nil

	case pubInline != "":
		// Register the caller's inline public key; dial with its private half.
		if opts.KeyFile == "" {
			return nil, "", false, fmt.Errorf("vultr: ssh_key given without a matching key_file for the dial")
		}
		s, perr := loadSignerFromFile(opts.KeyFile)
		if perr != nil {
			return nil, "", false, perr
		}
		id, cerr := c.CreateSSHKey(ctx, "agentflow-"+uuid.New().String()[:8], pubInline)
		if cerr != nil {
			return nil, "", false, fmt.Errorf("vultr register ssh key: %w", cerr)
		}
		return s, id, true, nil

	default:
		// Generate an ephemeral keypair, register the public half, dial with
		// the private half kept in memory.
		pub, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return nil, "", false, fmt.Errorf("vultr: generate keypair: %w", gerr)
		}
		s, perr := ssh.NewSignerFromKey(priv)
		if perr != nil {
			return nil, "", false, fmt.Errorf("vultr: signer: %w", perr)
		}
		sshPub, perr := ssh.NewPublicKey(pub)
		if perr != nil {
			return nil, "", false, fmt.Errorf("vultr: public key: %w", perr)
		}
		// MarshalAuthorizedKey yields "ssh-ed25519 AAAA... [comment]\n"; trim
		// the trailing newline so Vultr stores a clean key.
		pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
		id, cerr := c.CreateSSHKey(ctx, "agentflow-"+uuid.New().String()[:8], pubLine)
		if cerr != nil {
			return nil, "", false, fmt.Errorf("vultr register ssh key: %w", cerr)
		}
		return s, id, true, nil
	}
}

func (p *VultrProvider) waitForReady(ctx context.Context, c *vultrClient, instID string) (vultrInstance, error) {
	deadline := time.Now().Add(5 * time.Minute)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		inst, err := c.GetInstance(ctx, instID)
		if err != nil {
			return vultrInstance{}, fmt.Errorf("vultr get instance: %w", err)
		}
		if inst.isReady() {
			return inst, nil
		}
		if time.Now().After(deadline) {
			return vultrInstance{}, fmt.Errorf("vultr: instance %s did not become ready within 5m (status=%s power=%s server=%s ip=%s)",
				instID, inst.Status, inst.PowerStatus, inst.ServerStatus, inst.MainIP)
		}
		select {
		case <-ctx.Done():
			return vultrInstance{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *VultrProvider) Exec(ctx context.Context, handle *Handle, cmdStr string) (*ExecResult, error) {
	return sshExec(ctx, p.clientOf(handle), cmdStr)
}

func (p *VultrProvider) Read(ctx context.Context, handle *Handle, path string) ([]byte, error) {
	return sshRead(ctx, p.clientOf(handle), path)
}

func (p *VultrProvider) Write(ctx context.Context, handle *Handle, path string, content []byte) error {
	return sshWrite(ctx, p.clientOf(handle), path, content)
}

func (p *VultrProvider) Destroy(ctx context.Context, handle *Handle) error {
	client := p.clientOf(handle)
	_ = client.Close()

	meta := p.metaOf(handle)
	c := newVultrClient(p.apiKey, "")
	// Delete the instance with a short retry so a transient API error doesn't
	// leak a billed instance.
	if err := p.deleteWithRetry(ctx, c, meta.instanceID); err != nil {
		p.log.Error("vultr: failed to delete instance — manual cleanup required",
			"instance_id", meta.instanceID, "ip", meta.mainIP, "err", err)
		// Don't return the error: Destroy is best-effort during reap, and we
		// still want to mark the handle destroyed.
	}
	if meta.createdKey && meta.sshKeyID != "" {
		if err := c.DeleteSSHKey(ctx, meta.sshKeyID); err != nil {
			p.log.Warn("vultr: failed to delete ephemeral ssh key", "ssh_key_id", meta.sshKeyID, "err", err)
		}
	}
	handle.State = HandleDestroyed
	return nil
}

func (p *VultrProvider) Alive(handle *Handle) bool {
	return sshAlive(p.clientOf(handle))
}

// deleteWithRetry deletes a Vultr instance, retrying a few times on transient
// failures (network blips, 429) so an instance isn't left running.
func (p *VultrProvider) deleteWithRetry(ctx context.Context, c *vultrClient, instID string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.DeleteInstance(ctx, instID); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(500*(attempt+1)) * time.Millisecond):
		}
	}
	return lastErr
}

func (p *VultrProvider) clientOf(handle *Handle) *ssh.Client {
	return handle.internal.(*ssh.Client)
}

func (p *VultrProvider) metaOf(handle *Handle) vultrHandleMeta {
	m, _ := handle.Meta["instance_id"].(string)
	return vultrHandleMeta{
		instanceID: m,
		sshKeyID:   optString(handle.Meta, "ssh_key_id"),
		createdKey: optBool(handle.Meta, "created_key"),
		mainIP:     optString(handle.Meta, "main_ip"),
		user:       optString(handle.Meta, "user"),
	}
}

// loadSignerFromFile parses a private key file into an SSH signer.
func loadSignerFromFile(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ssh key %s: %w", path, err)
	}
	s, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key %s: %w", path, err)
	}
	return s, nil
}

// --- SpawnOpts.ShellOpts readers ---

func intOpt(m map[string]any, key string, dflt int) int {
	if m == nil {
		return dflt
	}
	v, ok := m[key]
	if !ok {
		return dflt
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return dflt
}

func stringSliceOpt(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func optBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
