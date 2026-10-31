package config

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const CredentialKeyFile = ".credentials-key"

// Sealed value formats. v1 binds a config.json credential to its member
// name (and a Seal value to the caller's name); v2 binds a credential to
// its full path and destination, and a database field to the context its
// owner supplies. Every format starts with sealedMarker.
const (
	sealedMarker   = "ether-sealed:"
	sealedPrefix   = "ether-sealed:v1:"
	sealedPrefixV2 = "ether-sealed:v2:"
)

func credentialKey(dir string, existing []byte) ([]byte, error) {
	path := filepath.Join(dir, CredentialKeyFile)
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("invalid credential key")
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	if ContainsSealed(existing) {
		return nil, errors.New("encrypted credentials require the original credential key; restore the complete backup")
	}
	key = make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return credentialKey(dir, existing)
	}
	if err != nil {
		return nil, err
	}
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return nil, err
	}
	if ce != nil {
		return nil, ce
	}
	return key, nil
}

// ProtectedValues returns the plaintext config strings the credential walker
// treats as secrets, including usernames, tokens, passwords, API keys,
// URL templates, secret-settings values, and the database URL.
// The result is for redaction. Callers must not write it into an archive.
func ProtectedValues(cfg Config) []string {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var out []string
	collectProtected(root, "", &out)
	return out
}

func collectProtected(v any, parent string, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			value := x[k]
			if inner, ok := value.(map[string]any); ok && k == "secretSettings" {
				for _, sk := range sortedKeys(inner) {
					if text, ok := inner[sk].(string); ok && text != "" {
						*out = append(*out, text)
					}
				}
				continue
			}
			if text, ok := value.(string); ok && text != "" && (secretField(k) || secretChild(parent, k)) {
				*out = append(*out, text)
				continue
			}
			collectProtected(value, k, out)
		}
	case []any:
		for _, value := range x {
			collectProtected(value, parent, out)
		}
	}
}

func secretField(k string) bool {
	switch strings.ToLower(k) {
	case "urltemplate", "token", "apikey", "password", "clientsecret", "username", "tmdbkey":
		return true
	}
	return false
}

// secretChild covers secrets whose key is only sensitive in context, such as
// the database connection URL (which embeds a password).
func secretChild(parent, k string) bool {
	return strings.EqualFold(parent, "database") && strings.EqualFold(k, "url")
}

// destinationFields are the non-secret members of an object that say where
// its secrets are sent. A v2 credential is bound to them, so pointing a
// provider at another host (or another provider kind) by editing the stored
// document leaves its sealed password unusable.
var destinationFields = []string{"baseUrl", "definition", "endpoint", "host", "kind", "port", "protocol", "provider", "server", "tls", "url"}

// credentialFormatKey is the top-level member a v2 document carries. A
// document with it was written entirely in v2, so a legacy (v1) value in
// it is a replayed old ciphertext and is refused.
const (
	credentialFormatKey = "credentialFormat"
	credentialFormatV2  = 2
)

// docStats describes an opened config document.
type docStats struct {
	strict    bool // carried the v2 marker
	plaintext int  // secrets stored unsealed
	legacy    int  // secrets sealed in v1
}

// credentialWalker seals or opens every secret of a config document.
type credentialWalker struct {
	aead  cipher.AEAD
	seal  bool
	bound bool // seal: write v2; open: the document is strict
	stats docStats
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealDocument seals every secret of the config document data. bound
// selects the v2 format (path- and destination-bound, with the format
// marker); otherwise values are written in the legacy v1 format that
// releases before v2 can read.
func sealDocument(data, key []byte, bound bool) ([]byte, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	w := &credentialWalker{aead: aead, seal: true, bound: bound}
	if err := w.walk(root, nil, ""); err != nil {
		return nil, err
	}
	if m, ok := root.(map[string]any); ok {
		delete(m, credentialFormatKey)
		if bound {
			m[credentialFormatKey] = credentialFormatV2
		}
	}
	return json.MarshalIndent(root, "", "  ")
}

// openDocument opens every sealed secret of the config document data and
// strips the format marker.
func openDocument(data, key []byte) ([]byte, docStats, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, docStats{}, err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, docStats{}, err
	}
	w := &credentialWalker{aead: aead}
	if m, ok := root.(map[string]any); ok {
		if v, present := m[credentialFormatKey]; present {
			n, _ := v.(float64)
			if n != credentialFormatV2 {
				return nil, docStats{}, fmt.Errorf("config: unsupported credential format %v; this release is older than the one that wrote the configuration", v)
			}
			w.bound = true
			delete(m, credentialFormatKey)
		}
	}
	w.stats.strict = w.bound
	if err := w.walk(root, nil, ""); err != nil {
		return nil, docStats{}, err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	return out, w.stats, err
}

// walk visits v at path; parent is the member name v belongs to. Each
// object's destination is computed from its non-secret members before any
// of its values change, so sealing and opening derive the same context
// whatever order the map is visited in.
func (w *credentialWalker) walk(v any, path []string, parent string) error {
	switch x := v.(type) {
	case map[string]any:
		dest := destination(x, parent)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			value := x[k]
			p := appendPath(path, k)
			if inner, ok := value.(map[string]any); ok && k == "secretSettings" {
				// Every value of a secret-settings map is a credential,
				// bound to the indexer that owns the map.
				for _, sk := range sortedKeys(inner) {
					text, ok := inner[sk].(string)
					if !ok || text == "" {
						continue
					}
					out, err := w.value(text, sk, appendPath(p, sk), dest)
					if err != nil {
						return err
					}
					inner[sk] = out
				}
				continue
			}
			if text, ok := value.(string); ok && text != "" && (secretField(k) || secretChild(parent, k)) {
				out, err := w.value(text, k, p, dest)
				if err != nil {
					return err
				}
				x[k] = out
			} else if err := w.walk(value, p, k); err != nil {
				return err
			}
		}
	case []any:
		for i, value := range x {
			if err := w.walk(value, appendPath(path, elementName(value, i)), parent); err != nil {
				return err
			}
		}
	}
	return nil
}

// value seals or opens one secret: name is its member name (the legacy v1
// context), path and dest its v2 context.
func (w *credentialWalker) value(text, name string, path []string, dest string) (string, error) {
	aad := credentialAAD(path, dest)
	if w.seal {
		if w.bound {
			return sealWithAAD(w.aead, sealedPrefixV2, text, aad)
		}
		return sealWithAAD(w.aead, sealedPrefix, text, []byte(name))
	}
	where := strings.Join(path, ".")
	switch {
	case strings.HasPrefix(text, sealedPrefixV2):
		plain, err := openWithAAD(w.aead, sealedPrefixV2, text, aad)
		if err != nil {
			return "", fmt.Errorf("decrypt credential %s: key mismatch, damaged configuration, or the value was moved to another setting or destination", where)
		}
		return plain, nil
	case strings.HasPrefix(text, sealedPrefix):
		plain, err := openWithAAD(w.aead, sealedPrefix, text, []byte(name))
		if err != nil {
			return "", fmt.Errorf("decrypt credential %s: key mismatch or damaged configuration", where)
		}
		// A release that predates v2 read a v2 value as an opaque string
		// and sealed it again in v1 when it saved: unwrap it.
		if strings.HasPrefix(plain, sealedPrefixV2) {
			inner, err := openWithAAD(w.aead, sealedPrefixV2, plain, aad)
			if err != nil {
				return "", fmt.Errorf("decrypt credential %s: key mismatch, damaged configuration, or the value was moved to another setting or destination", where)
			}
			return inner, nil
		}
		if w.bound {
			return "", fmt.Errorf("decrypt credential %s: legacy ciphertext in an upgraded configuration; re-enter the credential", where)
		}
		w.stats.legacy++
		return plain, nil
	case strings.HasPrefix(text, sealedMarker):
		return "", fmt.Errorf("decrypt credential %s: unsupported sealed format; this release is older than the one that wrote it", where)
	}
	w.stats.plaintext++
	return text, nil
}

// destination is the canonical form of an object's destination members.
func destination(m map[string]any, parent string) string {
	var b strings.Builder
	if parent == "posters" {
		enc, _ := json.Marshal(m["source"])
		b.WriteString("source=")
		b.Write(enc)
		b.WriteByte(0)
	}
	for _, k := range destinationFields {
		v, ok := m[k]
		if !ok || v == nil || secretField(k) || secretChild(parent, k) {
			continue
		}
		if _, nested := v.(map[string]any); nested {
			continue
		}
		if _, list := v.([]any); list {
			continue
		}
		enc, err := json.Marshal(v)
		if err != nil {
			continue
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.Write(enc)
		b.WriteByte(0)
	}
	return b.String()
}

// elementName names an array element in a credential path: its "id" when
// it has one (so reordering a list keeps its secrets valid), else its index.
func elementName(v any, i int) string {
	if m, ok := v.(map[string]any); ok {
		if id, ok := m["id"].(string); ok && id != "" {
			return "[id=" + id + "]"
		}
	}
	return "[" + strconv.Itoa(i) + "]"
}

func appendPath(path []string, k string) []string {
	return append(path[:len(path):len(path)], k)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// credentialAAD is the v2 additional data of a config.json credential.
func credentialAAD(path []string, dest string) []byte {
	return contextAAD("ether-credential:v2", append(append([]string(nil), path...), "\x00dest", dest))
}

// contextAAD encodes a domain and context parts unambiguously (each part
// length-prefixed).
func contextAAD(domain string, parts []string) []byte {
	b := append([]byte(domain), 0)
	for _, p := range parts {
		b = binary.AppendUvarint(b, uint64(len(p)))
		b = append(b, p...)
	}
	return b
}

func sealWithAAD(aead cipher.AEAD, prefix, plain string, aad []byte) (string, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return prefix + base64.RawStdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plain), aad)), nil
}

func openWithAAD(aead cipher.AEAD, prefix, sealed string, aad []byte) (string, error) {
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil || len(payload) < aead.NonceSize()+aead.Overhead() {
		return "", errors.New("invalid sealed credential")
	}
	plain, err := aead.Open(nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], aad)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ContainsSealed reports whether b holds a sealed value of any format, so
// that it needs the data directory's credential key to be read.
func ContainsSealed(b []byte) bool { return bytes.Contains(b, []byte(sealedMarker)) }

func clone(c Config) Config {
	b, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(b, &out)
	return out
}

// KeyFingerprint identifies a credential key without revealing it. Cluster
// nodes compare fingerprints to prove they hold the same key.
func KeyFingerprint(key []byte) string {
	h := sha256.New()
	h.Write([]byte("ether-credential-key-fingerprint:v1\x00"))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil))
}

// sealBytes encrypts plain with key, binding it to aad (a name that must
// match when opening). The result is printable.
func sealBytes(key []byte, aad string, plain []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return sealedPrefix + base64.RawStdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(aad))), nil
}

func openBytes(key []byte, aad, sealed string) ([]byte, error) {
	if !strings.HasPrefix(sealed, sealedPrefix) {
		return nil, errors.New("config: value is not sealed")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, sealedPrefix))
	if err != nil || len(payload) < aead.NonceSize() {
		return nil, errors.New("config: invalid sealed value")
	}
	plain, err := aead.Open(nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("config: decrypt %s: key mismatch or damaged value", aad)
	}
	return plain, nil
}

// InstallCredentialKey writes key as dir's credential key. It refuses to
// replace an existing key file: a node joining a cluster moves its old
// configuration aside first.
func InstallCredentialKey(dir string, key []byte) error {
	if len(key) != 32 {
		return errors.New("config: credential key must be 32 bytes")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, CredentialKeyFile)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("config: %s already exists", path)
	}
	return writeFileAtomic(path, key, 0o600)
}

// ReadCredentialKey returns dir's credential key, or os.ErrNotExist.
func ReadCredentialKey(dir string) ([]byte, error) {
	key, err := os.ReadFile(filepath.Join(dir, CredentialKeyFile))
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid credential key")
	}
	return key, nil
}
