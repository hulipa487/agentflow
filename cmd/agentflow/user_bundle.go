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
	"net/http"
	"os"
	"strings"
	"time"
)

func cmdUser(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: agentflow user <export|import|keygen> [flags]")
		return 2
	}
	switch args[0] {
	case "export":
		return cmdUserExport(args[1:], stdout, stderr)
	case "import":
		return cmdUserImport(args[1:], stdout, stderr)
	case "keygen":
		return cmdUserKeygen(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "user: unknown command", args[0])
		return 2
	}
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
	fs := flag.NewFlagSet("user keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prefix := fs.String("out", "agentflow-bundle", "output prefix; writes <prefix>.pub and <prefix>.key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
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
	fs := flag.NewFlagSet("user export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("url", "http://127.0.0.1:9090", "admin API base URL")
	token := fs.String("token", os.Getenv("ADMIN_TOKEN"), "admin bearer token (default $ADMIN_TOKEN)")
	id := fs.String("id", "", "user id to export")
	pubPath := fs.String("pubkey", "", "X25519 public key file (omit to export credential metadata only)")
	out := fs.String("out", "", "write the bundle zip here (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id == "" {
		fmt.Fprintln(stderr, "user export: -id is required")
		return 2
	}
	url := strings.TrimSuffix(*base, "/") + "/admin/api/users/" + *id + "/bundle"
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
	if *out == "" {
		_, _ = stdout.Write(doc)
		return 0
	}
	if err := os.WriteFile(*out, doc, 0o600); err != nil {
		fmt.Fprintln(stderr, "user export:", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s (%d bytes)\n", *out, len(doc))
	return 0
}

func cmdUserImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("url", "http://127.0.0.1:9090", "admin API base URL")
	token := fs.String("token", os.Getenv("ADMIN_TOKEN"), "admin bearer token (default $ADMIN_TOKEN)")
	keyPath := fs.String("key", "", "X25519 private key file (required when credentials are sealed)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "user import: exactly one bundle zip argument is required")
		return 2
	}
	bundlePath := fs.Arg(0)
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
	resp, err := adminRequest(http.MethodPost, strings.TrimSuffix(*base, "/")+"/admin/api/users/import", *token, &body, mw.FormDataContentType())
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
