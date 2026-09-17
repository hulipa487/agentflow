package main

import "testing"

// TestCheckConfigSource: -config and -configdir are mutually exclusive, but
// the default -config value alone (never explicitly passed) does not collide
// with -configdir.
func TestCheckConfigSource(t *testing.T) {
	if err := checkConfigSource(map[string]bool{"configdir": true}); err != nil {
		t.Fatalf("-configdir alone must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{"config": true}); err != nil {
		t.Fatalf("-config alone must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{}); err != nil {
		t.Fatalf("no config flags must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{"config": true, "configdir": true}); err == nil {
		t.Fatal("-config and -configdir together must fail")
	}
}
