package auth

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFileAndConstantTimeAuthentication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	contents := strings.Join([]string{
		`{"role":"admin","token":"root-secret","name":"root"}`,
		`{"role":"operator","token":"worker-secret","name":"worker"}`,
		`{"role":"reader","token":"read-secret","name":"reader"}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer worker-secret")
	p, err := a.Authenticate(req)
	if err != nil || p.Role != RoleOperator {
		t.Fatalf("authenticate = %#v, %v", p, err)
	}
	if Require(p, RoleAdmin) != ErrForbidden {
		t.Fatal("operator unexpectedly passed admin authorization")
	}
	req.Header.Set("Authorization", "Bearer wrong")
	if _, err := a.Authenticate(req); err != ErrUnauthorized {
		t.Fatalf("wrong token error = %v", err)
	}
}

func TestLoadFileRejectsDuplicateAndWorldReadableTokens(t *testing.T) {
	dir := t.TempDir()
	duplicate := filepath.Join(dir, "duplicate")
	if err := os.WriteFile(duplicate, []byte("{\"role\":\"reader\",\"token\":\"same\",\"name\":\"reader\"}\n{\"role\":\"admin\",\"token\":\"same\",\"name\":\"admin\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(duplicate); err == nil {
		t.Fatal("duplicate token value was accepted")
	}
	duplicateName := filepath.Join(dir, "duplicate-name")
	if err := os.WriteFile(duplicateName, []byte("{\"role\":\"reader\",\"token\":\"one\",\"name\":\"same\"}\n{\"role\":\"admin\",\"token\":\"two\",\"name\":\"same\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(duplicateName); err == nil {
		t.Fatal("duplicate token name was accepted")
	}
	world := filepath.Join(dir, "world")
	if err := os.WriteFile(world, []byte("{\"role\":\"reader\",\"token\":\"secret\",\"name\":\"reader\"}\n"), 0o604); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(world); err == nil {
		t.Fatal("world-readable token file was accepted")
	}
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(group, []byte("{\"role\":\"reader\",\"token\":\"secret\",\"name\":\"reader\"}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(group); err != nil {
		t.Fatalf("group-readable token file was rejected: %v", err)
	}
	groupWritable := filepath.Join(dir, "group-writable")
	if err := os.WriteFile(groupWritable, []byte("{\"role\":\"reader\",\"token\":\"secret\",\"name\":\"reader\"}\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(groupWritable, 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(groupWritable); err == nil {
		t.Fatal("group-writable token file was accepted")
	}
}

func TestLoadFileRejectsNonCanonicalAndAmbiguousRecords(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"role token":                          "admin secret",
		"token role":                          "secret admin",
		"colon":                               "admin:secret",
		"key-value":                           "role=admin token=secret",
		"bare token":                          "secret",
		"duplicate JSON field":                `{"role":"admin","token":"a","token":"b"}`,
		"unknown JSON field":                  `{"role":"admin","token":"a","value":"b"}`,
		"missing role":                        `{"token":"a"}`,
		"missing token":                       `{"role":"admin","name":"admin"}`,
		"missing name":                        `{"role":"admin","token":"a"}`,
		"role alias":                          `{"role":"read","token":"a"}`,
		"trailing JSON":                       `{"role":"admin","token":"a"}{"role":"reader","token":"b"}`,
		"non-object JSON":                     `[ {"role":"admin","token":"a"} ]`,
		"leading whitespace in token":         `{"role":"admin","token":" a"}`,
		"name with leading or trailing space": `{"role":"admin","token":"a","name":" actor"}`,
	}
	for name, line := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".tokens")
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(path); err == nil {
			t.Errorf("%s: accepted invalid token record %q", name, line)
		}
	}
}

func TestLoadFileRequiresExplicitNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	if err := os.WriteFile(path, []byte(`{"role":"operator","token":"worker-secret","name":"worker"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.tokens) != 1 {
		t.Fatalf("tokens = %d, want one", len(a.tokens))
	}
	if got := a.tokens[0].name; got != "worker" {
		t.Fatalf("explicit name = %q, want worker", got)
	}
	path = filepath.Join(dir, "missing-name")
	if err := os.WriteFile(path, []byte(`{"role":"operator","token":"worker-secret"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("token file without an explicit name was accepted")
	}
}

func TestNewUsesNeutralNames(t *testing.T) {
	a, err := New(map[string]Role{"worker-secret": RoleOperator, "root-secret": RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range a.tokens {
		if !strings.HasPrefix(entry.name, "configured-token-") {
			t.Fatalf("programmatic token name = %q, want neutral configured-token name", entry.name)
		}
	}
}

func TestCredentialLengthLimits(t *testing.T) {
	if _, err := New(map[string]Role{strings.Repeat("t", maxTokenLen+1): RoleReader}); err == nil {
		t.Fatal("oversized configured token was accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	record := `{"role":"reader","token":"ok","name":"` + strings.Repeat("n", maxNameLen+1) + `"}` + "\n"
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("oversized credential name was accepted")
	}

	a, err := New(map[string]Role{"ok": RoleReader})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", maxTokenLen+1))
	if _, err := a.Authenticate(req); err != ErrUnauthorized {
		t.Fatalf("oversized bearer header error = %v, want unauthorized", err)
	}
}
