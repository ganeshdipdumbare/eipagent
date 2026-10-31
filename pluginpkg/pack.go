package pluginpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"eipagent/engine"
	"eipagent/sdk"
)

// SignatureFile holds the package signature next to the manifest.
const SignatureFile = "plugin.sig"

const maxPackage = 8 << 20 // 8 MiB, compressed and uncompressed

func sdkVersion() string { return sdk.Version }

// Signature is an ed25519 signature over the canonical manifest bytes.
// The manifest carries every file's sha256, so it covers the package.
type Signature struct {
	Alg   string `json:"alg"` // "ed25519"
	KeyID string `json:"key_id"`
	Sig   string `json:"sig"` // base64
}

// Package is an opened .eip archive.
type Package struct {
	*Source
	Signature *Signature
	SHA256    string // of the archive bytes
}

// FileName is the conventional package file name.
func FileName(m *Manifest) string { return m.Name + "-" + m.Version + ".eip" }

// Build lints a plugin source and packs it. With a key the manifest is
// signed. It fails when the lint report has errors.
func Build(src *Source, key ed25519.PrivateKey) ([]byte, *Report, error) {
	if src.Manifest == nil {
		return nil, Lint(src), fmt.Errorf("plugin %s has no %s", src.Name, ManifestFile)
	}
	clean := *src.Manifest
	clean.Files = nil // recomputed below
	src = &Source{Name: src.Name, Manifest: &clean, Files: src.Files}
	rep := Lint(src)
	if err := rep.Err(); err != nil {
		return nil, rep, err
	}
	m := *src.Manifest
	m.Files = map[string]string{}
	for n, b := range src.Files {
		m.Files[n] = engine.Hash(b)
	}
	mb := m.Encode()
	files := map[string][]byte{ManifestFile: mb}
	if key != nil {
		sig := Signature{Alg: "ed25519", KeyID: KeyID(key.Public().(ed25519.PublicKey)), Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(key, mb))}
		sb, _ := json.MarshalIndent(sig, "", "  ")
		files[SignatureFile] = append(sb, '\n')
	}
	for n, b := range src.Files {
		files[n] = b
	}
	out, err := writeArchive(files)
	rep.Manifest = &m
	return out, rep, err
}

// writeArchive writes a reproducible tar.gz: sorted names, zero times.
func writeArchive(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { // manifest and signature first
		ri, rj := rank(names[i]), rank(names[j])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		b := files[n]
		h := &tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rank(n string) int {
	switch n {
	case ManifestFile:
		return 0
	case SignatureFile:
		return 1
	}
	return 2
}

// OpenFile opens a package from disk.
func OpenFile(p string) (*Package, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxPackage {
		return nil, fmt.Errorf("%s: package larger than %d bytes", p, maxPackage)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return Open(b)
}

// Open parses a package and checks its integrity: a flat list of regular
// files, each listed in the manifest with a matching sha256. It does not
// check the signature or lint; Verify does.
func Open(data []byte) (*Package, error) {
	if len(data) > maxPackage {
		return nil, fmt.Errorf("package larger than %d bytes", maxPackage)
	}
	sum := sha256.Sum256(data)
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("package: %w", err)
	}
	tr := tar.NewReader(io.LimitReader(gz, maxPackage+1))
	files := map[string][]byte{}
	total := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("package: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("package: %s is not a regular file", h.Name)
		}
		if h.Name != path.Base(h.Name) || h.Name == "." || h.Name == ".." || strings.ContainsAny(h.Name, `\:`) {
			return nil, fmt.Errorf("package: unsafe file name %q", h.Name)
		}
		if _, dup := files[h.Name]; dup {
			return nil, fmt.Errorf("package: duplicate file %s", h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxPackage+1))
		if err != nil {
			return nil, err
		}
		if total += len(b); total > maxPackage {
			return nil, fmt.Errorf("package: contents larger than %d bytes", maxPackage)
		}
		files[h.Name] = b
	}
	mb, ok := files[ManifestFile]
	if !ok {
		return nil, fmt.Errorf("package: no %s", ManifestFile)
	}
	m, err := ParseManifest(mb)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(mb, m.Encode()) {
		return nil, fmt.Errorf("package: %s is not in canonical form", ManifestFile)
	}
	pkg := &Package{Source: &Source{Name: m.Name, Manifest: m, Files: map[string][]byte{}}, SHA256: hex.EncodeToString(sum[:])}
	if sb, ok := files[SignatureFile]; ok {
		var s Signature
		if err := json.Unmarshal(sb, &s); err != nil {
			return nil, fmt.Errorf("package: %s: %w", SignatureFile, err)
		}
		pkg.Signature = &s
	}
	for n, b := range files {
		if n == ManifestFile || n == SignatureFile {
			continue
		}
		want, ok := m.Files[n]
		if !ok {
			return nil, fmt.Errorf("package: %s is not listed in the manifest", n)
		}
		if engine.Hash(b) != want {
			return nil, fmt.Errorf("package: %s sha256 does not match the manifest", n)
		}
		pkg.Files[n] = b
	}
	for n := range m.Files {
		if _, ok := pkg.Files[n]; !ok {
			return nil, fmt.Errorf("package: %s is listed in the manifest but missing", n)
		}
	}
	return pkg, nil
}

// VerifyOptions say what a package must satisfy before it is installed.
type VerifyOptions struct {
	// SHA256 pins the archive (e.g. from a lock or a control plane).
	SHA256 string
	// TrustedKeys are the publishers allowed to sign packages. A signed
	// package must verify against one of them when any are set.
	TrustedKeys []ed25519.PublicKey
	// RequireSignature refuses unsigned packages (secure mode).
	RequireSignature bool
}

// Verification is what Verify established.
type Verification struct {
	Report *Report
	Signer string // key id of the trusted key that signed it, or ""
}

// Verify checks the archive pin, the signature, compatibility and lint.
func Verify(pkg *Package, opts VerifyOptions) (*Verification, error) {
	if opts.SHA256 != "" && !strings.EqualFold(opts.SHA256, pkg.SHA256) {
		return nil, fmt.Errorf("%s: package sha256 %s does not match pin %s", pkg.Name, short(pkg.SHA256), short(opts.SHA256))
	}
	v := &Verification{}
	if pkg.Signature != nil && len(opts.TrustedKeys) > 0 {
		signer, err := checkSignature(pkg, opts.TrustedKeys)
		if err != nil {
			return nil, err
		}
		v.Signer = signer
	}
	if opts.RequireSignature && v.Signer == "" {
		if pkg.Signature == nil {
			return nil, fmt.Errorf("%s: package is not signed (signatures are required)", pkg.Name)
		}
		return nil, fmt.Errorf("%s: no trusted keys to verify the signature with", pkg.Name)
	}
	v.Report = Lint(pkg.Source)
	if err := v.Report.Err(); err != nil {
		return v, err
	}
	return v, nil
}

func checkSignature(pkg *Package, keys []ed25519.PublicKey) (string, error) {
	s := pkg.Signature
	if s.Alg != "ed25519" {
		return "", fmt.Errorf("%s: unsupported signature algorithm %q", pkg.Name, s.Alg)
	}
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil {
		return "", fmt.Errorf("%s: bad signature encoding", pkg.Name)
	}
	mb := pkg.Manifest.Encode()
	for _, k := range keys {
		if ed25519.Verify(k, mb, sig) {
			return KeyID(k), nil
		}
	}
	return "", fmt.Errorf("%s: signature (key %s) does not verify against any trusted key", pkg.Name, s.KeyID)
}

// ---- Keys ----

const (
	pubPrefix  = "eip-ed25519-pub:"
	privPrefix = "eip-ed25519-priv:"
)

// KeyID is a short fingerprint of a public key.
func KeyID(k ed25519.PublicKey) string {
	h := sha256.Sum256(k)
	return hex.EncodeToString(h[:8])
}

// GenerateKey makes a signing key pair, encoded for files.
func GenerateKey() (pub, priv string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return pubPrefix + base64.StdEncoding.EncodeToString(pk) + "\n", privPrefix + base64.StdEncoding.EncodeToString(sk.Seed()) + "\n", nil
}

// ParsePublicKey reads an "eip-ed25519-pub:..." key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), pubPrefix))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("not an eip ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// ParsePrivateKey reads an "eip-ed25519-priv:..." key.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), privPrefix))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("not an eip ed25519 private key")
	}
	return ed25519.NewKeyFromSeed(b), nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
