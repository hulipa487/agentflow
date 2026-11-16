// `agentflow user`: client commands for moving one profile between cells.
//
// The runtime exposes the bundle API; these commands are the operator-side
// wrapper, so the key never has to be pasted into a curl command by hand.
package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"agentflow/internal/config"
)

func cmdUser(args []string, stdout, stderr io.Writer) int {
	return runGroup("user", []subcommand{
		{"keygen", "write an X25519 key pair for sealing a bundle's credentials", cmdUserKeygen},
		{"export", "download one profile's bundle from a cell's admin API", cmdUserExport},
		{"import", "post a bundle to another cell's admin API", cmdUserImport},
	}, args, stdout, stderr)
}

// defaultAdminURL is where the admin API is when nothing says otherwise: the
// shared listener's default port, on this host.
const defaultAdminURL = "http://127.0.0.1:8080"

// addAdminFlags registers the flags every admin-API client command shares.
func addAdminFlags(fs *flag.FlagSet) (url, token, cfgPath *string) {
	url = fs.String("url", "", "admin API base URL (default: $AGENTFLOW_URL, then the config's gateway.listen, then "+defaultAdminURL+")")
	token = fs.String("token", "", "admin bearer token (default: $ADMIN_TOKEN)")
	cfgPath = addConfigFlag(fs)
	return url, token, cfgPath
}

// resolveAdmin picks the admin API base URL and token. The URL comes from
// -url, then $AGENTFLOW_URL, then the config's gateway.listen: the
// config named by -config, or the one discovery finds in the working
// directory. A named config that does not load is an error; a discovered one
// is only a guess, so it falls through to the default instead.
func resolveAdmin(url, token, cfgPath string) (string, string, error) {
	if token == "" {
		token = os.Getenv("ADMIN_TOKEN")
	}
	if url == "" {
		url = os.Getenv("AGENTFLOW_URL")
	}
	if url == "" {
		src, err := config.ResolveSource(cfgPath)
		if err == nil {
			var cfg *config.Config
			if cfg, err = src.Load(discardLogger()); err == nil {
				url, err = adminURLFromConfig(cfg)
			}
		}
		if err != nil && cfgPath != "" {
			return "", "", fmt.Errorf("admin address from %s: %w", cfgPath, err)
		}
	}
	if url == "" {
		url = defaultAdminURL
	}
	return strings.TrimSuffix(url, "/"), token, nil
}

// adminURLFromConfig turns gateway.listen, where the admin API is served, into
// a URL a client on this host can dial: an unspecified host (":8080",
// "0.0.0.0:8080") is loopback.
func adminURLFromConfig(cfg *config.Config) (string, error) {
	if !cfg.Runtime.Admin.TCPEnabled() {
		return "", fmt.Errorf("the admin API is served only on its socket (runtime.admin.tcp: false); pass -url")
	}
	listen := cfg.Gateway.Listen
	if listen == "" {
		return defaultAdminURL, nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("gateway.listen %q: %w", listen, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func adminRequest(method, url, token string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	return client.Do(req)
}

func readKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(raw))
	if b, err := base64.StdEncoding.DecodeString(text); err == nil {
		return b, nil
	}
	return []byte(text), nil
}

func cmdUserKeygen(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{
		Name:    "user keygen",
		Args:    "[PREFIX]",
		Summary: "Write an X25519 key pair to PREFIX.pub and PREFIX.key (default prefix: agentflow-bundle).",
		Detail:  "Give the public key to the exporting cell; keep the private key for the import.",
	}
	fs := newCommand(info, stderr)
	if code, ok := parseArgs(fs, args, map[string]string{
		"out": "pass the prefix as an argument: agentflow user keygen PREFIX",
	}); !ok {
		return code
	}
	if fs.NArg() > 1 {
		positionals(fs, info, 1)
		return 2
	}
	name := "agentflow-bundle"
	if fs.NArg() == 1 {
		name = fs.Arg(0)
	}
	prefix := &name
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(stderr, "keygen:", err)
		return 1
	}
	pubPath, keyPath := *prefix+".pub", *prefix+".key"
	if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())+"\n"), 0o644); err != nil {
		fmt.Fprintln(stderr, "keygen:", err)
		return 1
	}
	// The private key is the one secret here; 0600 from the start.
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(k.Bytes())+"\n"), 0o600); err != nil {
		fmt.Fprintln(stderr, "keygen:", err)
		return 1
	}
	fmt.Fprintf(stdout, "public key:  %s\nprivate key: %s\n", pubPath, keyPath)
	fmt.Fprintln(stdout, "give the public key to the exporting cell; keep the private key for the import")
	return 0
}

func cmdUserExport(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{
		Name:    "user export",
		Args:    "USER",
		Summary: "Download a full-fidelity bundle of one profile: identity, handles, memory, files and credentials.",
		Detail:  "With -pubkey the credentials are sealed to that key; without it only their metadata is exported.",
	}
	fs := newCommand(info, stderr)
	pubPath := fs.String("pubkey", "", "X25519 public key file to seal the credentials to")
	out := fs.String("out", "", "write the bundle zip to this file instead of stdout")
	urlFlag, tokenFlag, cfgPath := addAdminFlags(fs)
	if code, ok := parseArgs(fs, args, map[string]string{
		"id": "pass the user id as an argument: agentflow user export USER",
	}); !ok {
		return code
	}
	if !positionals(fs, info, 1) {
		return 2
	}
	id := fs.Arg(0)
	base, tok, err := resolveAdmin(*urlFlag, *tokenFlag, *cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "user export:", err)
		return 1
	}
	token := &tok
	url := base + "/admin/api/users/" + id + "/bundle"
	if *pubPath != "" {
		pub, err := readKey(*pubPath)
		if err != nil {
			fmt.Fprintln(stderr, "user export:", err)
			return 1
		}
		if len(pub) != 32 {
			fmt.Fprintln(stderr, "user export: public key must be 32 bytes")
			return 1
		}
		url += "?public_key=" + base64.StdEncoding.EncodeToString(pub)
	}
	resp, err := adminRequest(http.MethodGet, url, *token, nil, "")
	if err != nil {
		fmt.Fprintln(stderr, "user export:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(stderr, "user export: %s: %s\n", resp.Status, strings.TrimSpace(string(msg)))
		return 1
	}
	doc, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(stderr, "user export:", err)
		return 1
	}
	if err := writeOutput(*out, doc, 0o600, stdout); err != nil {
		fmt.Fprintln(stderr, "user export:", err)
		return 1
	}
	if *out != "" {
		fmt.Fprintf(stdout, "wrote %s (%d bytes)\n", *out, len(doc))
	}
	return 0
}

func cmdUserImport(args []string, stdout, stderr io.Writer) int {
	info := cmdInfo{
		Name:    "user import",
		Args:    "BUNDLE.zip",
		Summary: "Post a bundle to a cell's admin API, which re-encrypts its credentials with that cell's master key.",
		Detail:  "Without -key, sealed credentials are skipped and only their metadata is imported.",
	}
	fs := newCommand(info, stderr)
	keyPath := fs.String("key", "", "X25519 private key file the bundle's credentials are sealed to")
	urlFlag, tokenFlag, cfgPath := addAdminFlags(fs)
	if code, ok := parseArgs(fs, args, nil); !ok {
		return code
	}
	if !positionals(fs, info, 1) {
		return 2
	}
	bundlePath := fs.Arg(0)
	base, tok, err := resolveAdmin(*urlFlag, *tokenFlag, *cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "user import:", err)
		return 1
	}
	token := &tok
	doc, err := os.ReadFile(bundlePath)
	if err != nil {
		fmt.Fprintln(stderr, "user import:", err)
		return 1
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("bundle", "bundle.zip")
	if err != nil {
		fmt.Fprintln(stderr, "user import:", err)
		return 1
	}
	if _, err := fw.Write(doc); err != nil {
		fmt.Fprintln(stderr, "user import:", err)
		return 1
	}
	if *keyPath != "" {
		priv, err := readKey(*keyPath)
		if err != nil {
			fmt.Fprintln(stderr, "user import:", err)
			return 1
		}
		if len(priv) != 32 {
			fmt.Fprintln(stderr, "user import: private key must be 32 bytes")
			return 1
		}
		_ = mw.WriteField("private_key", base64.StdEncoding.EncodeToString(priv))
	}
	if err := mw.Close(); err != nil {
		fmt.Fprintln(stderr, "user import:", err)
		return 1
	}
	resp, err := adminRequest(http.MethodPost, base+"/admin/api/users/import", *token, &body, mw.FormDataContentType())
	if err != nil {
		fmt.Fprintln(stderr, "user import:", err)
		return 1
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "user import: %s: %s\n", resp.Status, strings.TrimSpace(string(msg)))
		return 1
	}
	_, _ = stdout.Write(msg)
	return 0
}
