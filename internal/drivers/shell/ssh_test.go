package shell

import "testing"

func TestShellQuote(t *testing.T) {
	got := shellQuote("/tmp/it's fine.txt")
	want := "'/tmp/it'\\''s fine.txt'"
	if got != want {
		t.Fatalf("shellQuote mismatch: got %q want %q", got, want)
	}
}

func TestSSHAuthRequiresCredential(t *testing.T) {
	_, err := sshAuth(SpawnOpts{Host: "localhost:22", User: "test"})
	if err == nil {
		t.Fatal("expected missing credential error")
	}
}

func TestSSHAuthPassword(t *testing.T) {
	auth, err := sshAuth(SpawnOpts{Host: "localhost:22", User: "test", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(auth) != 1 {
		t.Fatalf("expected 1 auth method, got %d", len(auth))
	}
}
