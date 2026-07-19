// Package auth implements file-backed bearer tokens for the HTTP API.
package auth

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type Role string

const (
	RoleReader   Role = "reader"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
	maxTokenLen       = 4096
	maxNameLen        = 200
)

type Principal struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
}

type token struct {
	name  string
	role  Role
	value string
}

type Authenticator struct {
	tokens []token
}

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

// New constructs an authenticator. Map keys are token values and values are
// roles; this helper is useful for tests and process-local configuration.
func New(values map[string]Role) (*Authenticator, error) {
	a := &Authenticator{}
	index := 0
	for value, role := range values {
		index++
		if err := a.add(token{name: fmt.Sprintf("configured-token-%d", index), value: strings.TrimSpace(value), role: role}); err != nil {
			return nil, err
		}
	}
	if len(a.tokens) == 0 {
		return nil, fmt.Errorf("no tokens configured")
	}
	return a, nil
}

// LoadFile reads one token record per line. Each non-blank line must be a JSON
// object with a token, a reader/operator/admin role, and a non-empty name. No
// other formats are accepted.
func LoadFile(path string) (*Authenticator, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open token file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat token file: %w", err)
	}
	// OpenShift Secret volumes use group-read (0440) so an arbitrary assigned
	// UID can read the file through its fsGroup. Group write/execute and every
	// other-user bit remain forbidden.
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o037 != 0 {
		return nil, fmt.Errorf("token file must be regular, group-read-only at most, and inaccessible to other users")
	}
	a := &Authenticator{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		entry, err := parseLine(text)
		if err != nil {
			return nil, fmt.Errorf("token file line %d: %w", line, err)
		}
		if err := a.add(entry); err != nil {
			return nil, fmt.Errorf("token file line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}
	if len(a.tokens) == 0 {
		return nil, fmt.Errorf("token file contains no tokens")
	}
	return a, nil
}

func parseLine(line string) (token, error) {
	if strings.TrimSpace(line) == "" {
		return token{}, fmt.Errorf("empty token record")
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		return token{}, fmt.Errorf("token record must be a JSON object")
	}

	fields, err := decodeJSONObject(line)
	if err != nil {
		return token{}, err
	}
	for key := range fields {
		switch key {
		case "name", "role", "token":
		default:
			return token{}, fmt.Errorf("unknown token record field %q", key)
		}
	}
	rawToken, ok := fields["token"]
	if !ok {
		return token{}, fmt.Errorf("token field is required")
	}
	rawRole, ok := fields["role"]
	if !ok {
		return token{}, fmt.Errorf("role field is required")
	}
	var value string
	if err := json.Unmarshal(rawToken, &value); err != nil {
		return token{}, fmt.Errorf("token field must be a string")
	}
	var role Role
	if err := json.Unmarshal(rawRole, &role); err != nil {
		return token{}, fmt.Errorf("role field must be a string")
	}
	if !isRole(role) {
		return token{}, fmt.Errorf("role must be reader, operator, or admin")
	}
	rawName, ok := fields["name"]
	if !ok {
		return token{}, fmt.Errorf("name field is required")
	}
	var name string
	if err := json.Unmarshal(rawName, &name); err != nil {
		return token{}, fmt.Errorf("name field must be a string")
	}
	if name == "" {
		return token{}, fmt.Errorf("name field must not be empty")
	}
	if name != strings.TrimSpace(name) {
		return token{}, fmt.Errorf("name must not have leading or trailing whitespace")
	}
	return token{name: name, value: value, role: role}, nil
}

// decodeJSONObject reads exactly one JSON object and rejects duplicate fields.
func decodeJSONObject(line string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(line))
	first, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON token record: %w", err)
	}
	delim, ok := first.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("token record must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON token record: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("token record object key must be a string")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate token record field %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("invalid value for token record field %q: %w", key, err)
		}
		fields[key] = value
	}
	last, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON token record: %w", err)
	}
	if delim, ok := last.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("token record must be a JSON object")
	}
	if extra, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("token record contains trailing JSON")
		}
		return nil, fmt.Errorf("invalid trailing data in token record: %w", err)
	} else if extra != nil {
		return nil, fmt.Errorf("token record contains trailing JSON")
	}
	return fields, nil
}

func (a *Authenticator) add(t token) error {
	if t.value != strings.TrimSpace(t.value) {
		return fmt.Errorf("token must not have leading or trailing whitespace")
	}
	if t.value == "" {
		return fmt.Errorf("empty token")
	}
	if len(t.value) > maxTokenLen {
		return fmt.Errorf("token exceeds %d bytes", maxTokenLen)
	}
	if !isRole(t.role) {
		return fmt.Errorf("role must be reader, operator, or admin")
	}
	if t.name == "" {
		return fmt.Errorf("name is required")
	}
	if len(t.name) > maxNameLen {
		return fmt.Errorf("name exceeds %d bytes", maxNameLen)
	}
	digest := tokenDigest(t.value)
	for _, existing := range a.tokens {
		if existing.name == t.name {
			return fmt.Errorf("duplicate token name")
		}
		existingDigest := tokenDigest(existing.value)
		if hmac.Equal(digest[:], existingDigest[:]) {
			return fmt.Errorf("duplicate token value")
		}
	}
	a.tokens = append(a.tokens, t)
	return nil
}

func isRole(role Role) bool {
	switch role {
	case RoleReader, RoleOperator, RoleAdmin:
		return true
	default:
		return false
	}
}

func rank(role Role) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleReader:
		return 1
	default:
		return 0
	}
}

// Authenticate hashes the Authorization credential to a fixed-size value and
// compares those digests in constant time. Fixing the length before comparison
// avoids a length-dependent raw-secret comparison.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	if a == nil {
		return Principal{}, ErrUnauthorized
	}
	rawHeader := r.Header.Get("Authorization")
	if len(rawHeader) > len("Bearer ")+maxTokenLen {
		return Principal{}, ErrUnauthorized
	}
	header := strings.TrimSpace(rawHeader)
	if len(header) < len("Bearer ") || !strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
		return Principal{}, ErrUnauthorized
	}
	provided := strings.TrimSpace(header[len("Bearer "):])
	if provided == "" || strings.ContainsAny(provided, " \t\r\n") {
		return Principal{}, ErrUnauthorized
	}
	providedSum := tokenDigest(provided)
	for _, candidate := range a.tokens {
		candidateSum := tokenDigest(candidate.value)
		if hmac.Equal(providedSum[:], candidateSum[:]) {
			return Principal{Name: candidate.name, Role: candidate.role}, nil
		}
	}
	return Principal{}, ErrUnauthorized
}

func tokenDigest(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}

func Require(principal Principal, minimum Role) error {
	if rank(principal.Role) < rank(minimum) {
		return ErrForbidden
	}
	return nil
}
