package main

// Support bundle: a redacted, self-contained snapshot an operator can attach to
// an issue. It contains the effective doctor result and configuration text
// with secret-looking values replaced, never a credential value.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"agentflow/internal/vm"
)

// secretKV matches the value half of a YAML/JSON/env line whose key looks like
// a secret. It is intentionally broad: a false positive costs a redacted
// non-secret, a false negative leaks a key.
var secretKV = regexp.MustCompile(`(?i)^(\s*[-\w.]*(?:key|token|secret|password|passwd|dsn|authorization|access|private)[-\w.]*\s*[:=]\s*)(.*)$`)

func redactConfig(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if m := secretKV.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + `"***"`
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

func cmdSupport(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath, configDir := cliConfigFlags("support-bundle", stderr)
	out := fs.String("out", "", "output zip path (default agentflow-support-<timestamp>.zip)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "support-bundle: unexpected argument:", fs.Arg(0))
		return 2
	}
	cfg, src, err := loadConfigForCLI(*cfgPath, *configDir, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "support-bundle: config:", err)
		return 1
	}
	if *out == "" {
		*out = "agentflow-support-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(stderr, "support-bundle:", err)
		return 1
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	write := func(name string, body []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}

	var doctor bytes.Buffer
	doctorArgs := []string{"-config", src.String()}
	_ = cmdDoctor(doctorArgs, &doctor, &doctor)

	var readme bytes.Buffer
	fmt.Fprintf(&readme, "AgentFlow support bundle\n")
	fmt.Fprintf(&readme, "created: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&readme, "version: %s\nprelude: %d\ngo: %s\n", version, vm.PreludeVersion, runtime.Version())
	fmt.Fprintf(&readme, "config: %s\nepoch: %s\n", src.String(), cfg.Epoch)
	fmt.Fprintf(&readme, "secrets: redacted; never attach a raw config\n")
	if err := write("README.txt", readme.Bytes()); err != nil {
		fmt.Fprintln(stderr, "support-bundle:", err)
		return 1
	}
	if err := write("doctor.txt", doctor.Bytes()); err != nil {
		fmt.Fprintln(stderr, "support-bundle:", err)
		return 1
	}
	if err := write("version.txt", []byte(version+"\nprelude "+fmt.Sprint(vm.PreludeVersion)+"\ngo "+runtime.Version()+"\n")); err != nil {
		fmt.Fprintln(stderr, "support-bundle:", err)
		return 1
	}

	if src.IsDir {
		err = filepath.Walk(src.Dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json", ".env":
			default:
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(src.Dir, path)
			return write(filepath.ToSlash(filepath.Join("config", rel)), redactConfig(b))
		})
		if err != nil {
			fmt.Fprintln(stderr, "support-bundle:", err)
			return 1
		}
	} else {
		b, err := os.ReadFile(src.Path)
		if err != nil {
			fmt.Fprintln(stderr, "support-bundle:", err)
			return 1
		}
		if err := write("config/"+filepath.Base(src.Path), redactConfig(b)); err != nil {
			fmt.Fprintln(stderr, "support-bundle:", err)
			return 1
		}
	}

	// Environment variable names only. Values are never read.
	names := []string{}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(name)
		if strings.Contains(up, "AGENTFLOW") || strings.Contains(up, "ADMIN_TOKEN") ||
			strings.Contains(up, "CREDENTIAL") || strings.HasSuffix(up, "_API_KEY") ||
			strings.Contains(up, "POSTGRES") || strings.Contains(up, "DATABASE") ||
			strings.Contains(up, "REDIS") || strings.Contains(up, "AWS") ||
			strings.Contains(up, "OPENAI") || strings.Contains(up, "ANTHROPIC") ||
			strings.Contains(up, "GEMINI") || strings.Contains(up, "TELEGRAM") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if err := write("env-names.txt", []byte(strings.Join(names, "\n")+"\n")); err != nil {
		fmt.Fprintln(stderr, "support-bundle:", err)
		return 1
	}

	fmt.Fprintln(stdout, "wrote", *out)
	return 0
}
