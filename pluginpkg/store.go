package pluginpkg

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/eipagent/engine"
)

// State is where a plugin version is in its lifecycle:
//
//	source dir:  DRAFT --lint ok--> VALIDATED
//	store:       install --> VALIDATED --approve--> APPROVED --enable--> ACTIVE
//	                                                    ^                  |
//	                                                    +-- disable -------+
//	             ACTIVE --start fails--> FAILED (previous version keeps running)
//	             any --quarantine--> QUARANTINED --approve--> APPROVED
//
// Installed versions are held outside the plugin directory the engine
// loads from, so nothing runs until it is approved and enabled.
type State string

const (
	StateDraft       State = "DRAFT"
	StateValidated   State = "VALIDATED"
	StateApproved    State = "APPROVED"
	StateActive      State = "ACTIVE"
	StateQuarantined State = "QUARANTINED"
	StateFailed      State = "FAILED"
)

var (
	ErrNotFound      = errors.New("plugin not installed")
	ErrNotApproved   = errors.New("plugin version is not approved")
	ErrVersionExists = errors.New("plugin version already installed")
)

// Runtime is a live engine the store activates plugins in. Activate
// must pin hash, (re)load the plugin and return an error unless it ends
// up ACTIVE; a failed update must leave the previous version running.
type Runtime interface {
	Activate(name, sourceHash string) error
	Deactivate(name string) error
}

// Version is one installed version of a plugin.
type Version struct {
	Version     string    `json:"version"`
	State       State     `json:"state"`
	SHA256      string    `json:"sha256"`        // of the package archive
	Source      string    `json:"source_sha256"` // of the .go source (the engine pin)
	Signer      string    `json:"signer,omitempty"`
	Manifest    *Manifest `json:"manifest"`
	InstalledAt time.Time `json:"installed_at"`
	InstalledBy string    `json:"installed_by,omitempty"`
	ApprovedBy  string    `json:"approved_by,omitempty"`
	ApprovedAt  time.Time `json:"approved_at,omitzero"`
	Reason      string    `json:"reason,omitempty"` // quarantine reason
	Error       string    `json:"error,omitempty"`  // last activation error
}

// Plugin is every installed version of one plugin.
type Plugin struct {
	Name     string              `json:"name"`
	Active   string              `json:"active,omitempty"`   // version
	Previous string              `json:"previous,omitempty"` // rollback target
	Versions map[string]*Version `json:"versions"`
}

// clone is a deep copy: callers can't change the store's state through
// what Get / List return (only Approve, Enable ... change it).
func (p *Plugin) clone() *Plugin {
	cp := *p
	cp.Versions = make(map[string]*Version, len(p.Versions))
	for k, v := range p.Versions {
		vc := *v
		if v.Manifest != nil {
			m := *v.Manifest
			m.Permissions = append([]string(nil), v.Manifest.Permissions...)
			m.Requires = append([]string(nil), v.Manifest.Requires...)
			m.Inject = append([]string(nil), v.Manifest.Inject...)
			m.Provides = append([]string(nil), v.Manifest.Provides...)
			m.Secrets = append([]string(nil), v.Manifest.Secrets...)
			m.Tools = append([]string(nil), v.Manifest.Tools...)
			if v.Manifest.Files != nil {
				m.Files = make(map[string]string, len(v.Manifest.Files))
				for f, h := range v.Manifest.Files {
					m.Files[f] = h
				}
			}
			vc.Manifest = &m
		}
		cp.Versions[k] = &vc
	}
	return &cp
}

// StoreOptions configure what the store accepts.
type StoreOptions struct {
	Verify VerifyOptions
	// AutoApprove approves every verified install (dev only).
	AutoApprove bool
	// AutoApproveSigned approves installs signed by a trusted key.
	AutoApproveSigned bool
	// Reserved names can't be installed (e.g. the built-in plugins).
	Reserved []string
	// Runtime, when set, activates plugins in a running engine.
	Runtime Runtime
	// Audit receives lifecycle events (plugin.install, plugin.enable ...).
	Audit func(kind string, data map[string]any)
}

// Store keeps installed plugin packages under a directory:
//
//	dir/state.json            lifecycle state of every version
//	dir/packages/NAME/VER.eip the verified archives
//	dir/active/NAME.go        what the engine loads (WithPluginDir)
//	dir/eip.lock              pins for the active sources
type Store struct {
	dir      string
	opts     StoreOptions
	mu       sync.Mutex
	installM sync.Mutex
	installs map[string]*sync.Mutex
	st       struct {
		Plugins map[string]*Plugin `json:"plugins"`
	}
}

// OpenStore opens (or creates) a store.
func OpenStore(dir string, opts StoreOptions) (*Store, error) {
	s := &Store{dir: dir, opts: opts}
	for _, d := range []string{dir, s.ActiveDir(), filepath.Join(dir, "packages")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	s.st.Plugins = map[string]*Plugin{}
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, fmt.Errorf("store state: %w", err)
		}
	}
	return s, nil
}

// SetRuntime attaches a live engine (done by eipagent.WithPluginStore).
func (s *Store) SetRuntime(r Runtime) { s.mu.Lock(); s.opts.Runtime = r; s.mu.Unlock() }

// Reserve adds names that can't be installed.
func (s *Store) Reserve(names ...string) {
	s.mu.Lock()
	s.opts.Reserved = append(s.opts.Reserved, names...)
	s.mu.Unlock()
}

// SetAudit sets where lifecycle events go.
func (s *Store) SetAudit(fn func(string, map[string]any)) {
	s.mu.Lock()
	s.opts.Audit = fn
	s.mu.Unlock()
}

func (s *Store) ActiveDir() string { return filepath.Join(s.dir, "active") }
func (s *Store) LockPath() string  { return filepath.Join(s.dir, "eip.lock") }
func (s *Store) Dir() string       { return s.dir }

// nameLock returns the per-plugin lock that serializes version allocation
// and install for one name.
func (s *Store) nameLock(name string) *sync.Mutex {
	s.installM.Lock()
	defer s.installM.Unlock()
	if s.installs == nil {
		s.installs = map[string]*sync.Mutex{}
	}
	l := s.installs[name]
	if l == nil {
		l = &sync.Mutex{}
		s.installs[name] = l
	}
	return l
}

// Pins returns the sha256 of every active source (name -> hash).
func (s *Store) Pins() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for n, p := range s.st.Plugins {
		if v := p.Versions[p.Active]; v != nil {
			out[n] = v.Source
		}
	}
	return out
}

// ActiveNames lists the plugins that are enabled.
func (s *Store) ActiveNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n, p := range s.st.Plugins {
		if p.Active != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// List returns every installed plugin.
func (s *Store) List() []Plugin {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Plugin
	for _, p := range s.st.Plugins {
		out = append(out, *p.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns one plugin's record.
func (s *Store) Get(name string) (*Plugin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.st.Plugins[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return p.clone(), nil
}

// Install verifies a package and stores it. It does not run anything:
// the version is VALIDATED (held until approved) or, with auto-approve,
// APPROVED. Reinstalling the same version with the same bytes is a no-op.
func (s *Store) Install(data []byte, by string) (*Version, error) {
	pkg, err := Open(data)
	if err != nil {
		return nil, err
	}
	m := pkg.Manifest
	s.mu.Lock()
	reserved := contains(s.opts.Reserved, m.Name)
	s.mu.Unlock()
	if reserved {
		return nil, fmt.Errorf("%s: name is reserved for a built-in plugin", m.Name)
	}
	ver, err := Verify(pkg, s.opts.Verify)
	if err != nil {
		s.audit("plugin.install_refused", map[string]any{"plugin": m.Name, "version": m.Version, "sha256": pkg.SHA256, "error": err.Error(), "by": by})
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if contains(s.opts.Reserved, m.Name) { // re-check: Reserve may have run during Verify
		return nil, fmt.Errorf("%s: name is reserved for a built-in plugin", m.Name)
	}
	p := s.st.Plugins[m.Name]
	if p == nil {
		p = &Plugin{Name: m.Name, Versions: map[string]*Version{}}
		s.st.Plugins[m.Name] = p
	}
	if old := p.Versions[m.Version]; old != nil {
		if old.SHA256 == pkg.SHA256 {
			cp := *old
			return &cp, nil
		}
		return nil, fmt.Errorf("%s %s is already installed with different contents; bump the version: %w", m.Name, m.Version, ErrVersionExists)
	}
	dir := filepath.Join(s.dir, "packages", m.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, m.Version+".eip"), data); err != nil {
		return nil, err
	}
	v := &Version{Version: m.Version, State: StateValidated, SHA256: pkg.SHA256, Source: engine.Hash(pkg.Code()),
		Signer: ver.Signer, Manifest: m, InstalledAt: time.Now().UTC(), InstalledBy: by}
	if s.opts.AutoApprove || (s.opts.AutoApproveSigned && ver.Signer != "") {
		v.State, v.ApprovedBy, v.ApprovedAt = StateApproved, "auto", time.Now().UTC()
	}
	p.Versions[m.Version] = v
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	s.audit("plugin.install", map[string]any{"plugin": m.Name, "version": m.Version, "sha256": pkg.SHA256, "signer": ver.Signer,
		"state": string(v.State), "permissions": m.Permissions, "by": by})
	cp := *v
	return &cp, nil
}

// Approve lets a VALIDATED or QUARANTINED version be enabled.
func (s *Store) Approve(name, version, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.versionLocked(name, version)
	if err != nil {
		return err
	}
	switch v.State {
	case StateValidated, StateQuarantined, StateFailed:
		v.State, v.ApprovedBy, v.ApprovedAt, v.Reason = StateApproved, by, time.Now().UTC(), ""
	case StateApproved, StateActive:
		return nil
	default:
		return fmt.Errorf("%s %s is %s and can't be approved", name, version, v.State)
	}
	s.audit("plugin.approve", map[string]any{"plugin": name, "version": version, "by": by})
	return s.saveLocked()
}

// Enable activates an approved version ("" = the newest approved one).
// If the plugin is already active on another version this is an update:
// when the new version fails to start, the old one keeps running and the
// new one is marked FAILED.
func (s *Store) Enable(name, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.st.Plugins[name]
	if !ok {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if version == "" {
		if p.Active != "" {
			// Never downgrade an active plugin by accident: only a newer
			// approved version is eligible.
			version = newestAfter(p, StateApproved, p.Active)
			if version == "" {
				return nil
			}
		} else {
			version = newest(p, StateApproved)
			if version == "" {
				return fmt.Errorf("%s: no approved version to enable", name)
			}
		}
	}
	v, ok := p.Versions[version]
	if !ok {
		return fmt.Errorf("%s %s: %w", name, version, ErrNotFound)
	}
	if v.State == StateActive {
		return nil
	}
	if v.State != StateApproved {
		return fmt.Errorf("%s %s is %s: %w", name, version, v.State, ErrNotApproved)
	}
	// Re-verify the stored archive in full, as at install: integrity, the
	// sha256 recorded at install, and the signature against the keys
	// trusted now. A package (or state.json) changed at rest, or one whose
	// signing key has since been removed, is quarantined instead of loaded.
	pkg, err := s.openLocked(name, version)
	if err == nil && pkg.SHA256 != v.SHA256 {
		err = fmt.Errorf("stored package changed since install")
	}
	if err == nil {
		opts := s.opts.Verify
		opts.SHA256 = v.SHA256
		var ver *Verification
		if ver, err = Verify(pkg, opts); err == nil && v.Signer != "" && ver.Signer != v.Signer {
			err = fmt.Errorf("signed by %q, but %q signed it at install", ver.Signer, v.Signer)
		}
	}
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return err // transient I/O, not an integrity failure
		}
		v.State, v.Reason = StateQuarantined, err.Error()
		s.saveLocked()
		s.audit("plugin.quarantine", map[string]any{"plugin": name, "version": version, "reason": v.Reason, "by": "store"})
		return fmt.Errorf("%s %s quarantined: %w", name, version, err)
	}
	activeFile := filepath.Join(s.ActiveDir(), name+".go")
	oldSrc, hadOld := []byte(nil), false
	if b, err := os.ReadFile(activeFile); err == nil {
		oldSrc, hadOld = b, true
	}
	if err := writeFileAtomic(activeFile, pkg.Code()); err != nil {
		return err
	}
	prevVer := p.Active
	p.Active = version
	if err := s.writeLockLocked(); err != nil {
		// Put the previous source and pin back before returning.
		p.Active = prevVer
		var restoreErr error
		if hadOld {
			restoreErr = writeFileAtomic(activeFile, oldSrc)
		} else {
			restoreErr = os.Remove(activeFile)
		}
		lockErr := s.writeLockLocked()
		return errors.Join(err, restoreErr, lockErr)
	}
	if rt := s.opts.Runtime; rt != nil {
		if err := rt.Activate(name, v.Source); err != nil {
			// Put the previous source and pin back; the engine kept the
			// previous instance running.
			var restoreErr error
			if hadOld {
				restoreErr = writeFileAtomic(activeFile, oldSrc)
			} else {
				restoreErr = os.Remove(activeFile)
			}
			p.Active = prevVer
			lockErr := s.writeLockLocked()
			v.State = StateFailed
			v.Error = err.Error()
			if restoreErr != nil || lockErr != nil {
				v.Error = fmt.Sprintf("store inconsistent after a failed enable: %v; %v", restoreErr, lockErr)
			}
			saveErr := s.saveLocked()
			s.audit("plugin.enable_failed", map[string]any{"plugin": name, "version": version, "error": v.Error, "kept": prevVer})
			return errors.Join(err, restoreErr, lockErr, saveErr)
		}
	}
	if prev := p.Versions[prevVer]; prev != nil && prevVer != version {
		prev.State = StateApproved
		p.Previous = prevVer
	}
	v.State, v.Error = StateActive, ""
	s.audit("plugin.enable", map[string]any{"plugin": name, "version": version, "previous": prevVer, "sha256": v.Source})
	return s.saveLocked()
}

// Disable stops a plugin; its version stays APPROVED.
func (s *Store) Disable(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disableLocked(name, "")
}

func (s *Store) disableLocked(name string, newState State) error {
	p, ok := s.st.Plugins[name]
	if !ok {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if p.Active == "" {
		return nil
	}
	if rt := s.opts.Runtime; rt != nil {
		if err := rt.Deactivate(name); err != nil {
			return err
		}
	}
	os.Remove(filepath.Join(s.ActiveDir(), name+".go"))
	old := p.Active
	p.Active = ""
	if err := s.writeLockLocked(); err != nil {
		// Keep the in-memory state in step with the pin on disk.
		p.Active = old
		lockErr := s.writeLockLocked()
		return errors.Join(err, lockErr)
	}
	if v := p.Versions[old]; v != nil {
		v.State = StateApproved
		if newState != "" {
			v.State = newState
		}
	}
	s.audit("plugin.disable", map[string]any{"plugin": name, "version": old})
	return s.saveLocked()
}

// Update installs a new version and, if the plugin is enabled and the
// new version is approved, switches to it (rolling back on failure).
// An unapproved update waits in VALIDATED; enable it after approval.
func (s *Store) Update(data []byte, by string) (*Version, error) {
	v, err := s.Install(data, by)
	if err != nil {
		return nil, err
	}
	p, err := s.Get(v.Manifest.Name)
	if err != nil { // uninstalled concurrently
		return v, err
	}
	if p.Active == "" || p.Active == v.Version || v.State != StateApproved {
		return v, nil
	}
	if err := s.Enable(v.Manifest.Name, v.Version); err != nil {
		return v, err
	}
	v.State = StateActive
	return v, nil
}

// Rollback re-enables the previous version.
func (s *Store) Rollback(name string) error {
	p, err := s.Get(name)
	if err != nil {
		return err
	}
	if p.Previous == "" {
		return fmt.Errorf("%s: no previous version to roll back to", name)
	}
	s.audit("plugin.rollback", map[string]any{"plugin": name, "from": p.Active, "to": p.Previous})
	return s.Enable(name, p.Previous)
}

// Quarantine pulls a version from service (disabling it if active); it
// can't be enabled again until approved.
func (s *Store) Quarantine(name, version, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version == "" {
		return fmt.Errorf("%s: a version is required to quarantine", name)
	}
	v, err := s.versionLocked(name, version)
	if err != nil {
		return err
	}
	if s.st.Plugins[name].Active == version {
		if err := s.disableLocked(name, StateQuarantined); err != nil {
			return err
		}
	}
	v.State, v.Reason = StateQuarantined, reason
	s.audit("plugin.quarantine", map[string]any{"plugin": name, "version": version, "reason": reason})
	return s.saveLocked()
}

// Uninstall disables a plugin and deletes every version.
func (s *Store) Uninstall(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.disableLocked(name, ""); err != nil {
		return err
	}
	delete(s.st.Plugins, name)
	os.RemoveAll(filepath.Join(s.dir, "packages", name))
	s.audit("plugin.uninstall", map[string]any{"plugin": name})
	return s.saveLocked()
}

// Package returns a stored package (for inspect).
func (s *Store) Package(name, version string) (*Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked(name, version)
}

func (s *Store) openLocked(name, version string) (*Package, error) {
	return OpenFile(filepath.Join(s.dir, "packages", name, version+".eip"))
}

func (s *Store) versionLocked(name, version string) (*Version, error) {
	p, ok := s.st.Plugins[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if version == "" {
		version = p.Active
		if version == "" {
			version = newestAny(p)
		}
	}
	v, ok := p.Versions[version]
	if !ok {
		return nil, fmt.Errorf("%s %s: %w", name, version, ErrNotFound)
	}
	return v, nil
}

func (s *Store) audit(kind string, data map[string]any) {
	if s.opts.Audit != nil {
		s.opts.Audit(kind, data)
	}
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(&s.st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "state.json"), append(b, '\n'))
}

func (s *Store) writeLockLocked() error {
	l := &engine.Lock{Plugins: map[string]string{}}
	for n, p := range s.st.Plugins {
		if v := p.Versions[p.Active]; v != nil {
			l.Plugins[n] = v.Source
		}
	}
	return engine.WriteLock(s.LockPath(), l)
}

func newest(p *Plugin, st State) string {
	best := ""
	for ver, v := range p.Versions {
		if v.State == st && (best == "" || semverLess(best, ver)) {
			best = ver
		}
	}
	return best
}

func newestAny(p *Plugin) string {
	best := ""
	for ver := range p.Versions {
		if best == "" || semverLess(best, ver) {
			best = ver
		}
	}
	return best
}

// newestAfter is newest, restricted to versions strictly newer than after.
func newestAfter(p *Plugin, st State, after string) string {
	best := ""
	for ver, v := range p.Versions {
		if v.State != st || !semverLess(after, ver) {
			continue
		}
		if best == "" || semverLess(best, ver) {
			best = ver
		}
	}
	return best
}

type parsedSemver struct {
	nums [3]int
	pre  []string
}

func parseSemverFull(s string) (parsedSemver, error) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return parsedSemver{}, fmt.Errorf("%q is not semver", s)
	}
	var v parsedSemver
	for i := range v.nums {
		v.nums[i], _ = strconv.Atoi(m[i+1])
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, nil
}

// compareSemver implements semver precedence, including prerelease
// ordering (1.0.0 > 1.0.0-rc.1; rc.2 > rc.10 numerically; numeric
// identifiers sort before alphanumeric ones).
func compareSemver(a, b parsedSemver) int {
	for i := range a.nums {
		if a.nums[i] != b.nums[i] {
			if a.nums[i] < b.nums[i] {
				return -1
			}
			return 1
		}
	}
	if len(a.pre) == 0 && len(b.pre) == 0 {
		return 0
	}
	if len(a.pre) == 0 {
		return 1
	}
	if len(b.pre) == 0 {
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case xerr == nil:
			return -1 // numeric identifiers have lower precedence
		case yerr == nil:
			return 1
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	if len(a.pre) < len(b.pre) {
		return -1
	}
	if len(a.pre) > len(b.pre) {
		return 1
	}
	return 0
}

func semverLess(a, b string) bool {
	va, ea := parseSemverFull(a)
	vb, eb := parseSemverFull(b)
	if ea != nil || eb != nil {
		return a < b
	}
	if c := compareSemver(va, vb); c != 0 {
		return c < 0
	}
	return a < b // deterministic tie-break on the full version string
}

func writeFileAtomic(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// TrustedKeys parses public keys (one per string).
func TrustedKeys(keys ...string) ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for _, k := range keys {
		pk, err := ParsePublicKey(k)
		if err != nil {
			return nil, err
		}
		out = append(out, pk)
	}
	return out, nil
}

// InstallSource installs a plugin submitted as source text (from an admin
// UI or an API). It drafts the manifest from the code (description from
// the doc comment, Requires and Permissions from the variables), gives it
// the next version (0.1.0, then the newest installed version + 1 patch),
// lints it without running it, signs it with key and installs it. The
// version is held as VALIDATED until approved. The lint report comes back
// even when installing fails, so a UI can show what to fix.
func (s *Store) InstallSource(name string, code []byte, key ed25519.PrivateKey, by string) (*Version, *Report, error) {
	if !nameRe.MatchString(name) {
		return nil, nil, fmt.Errorf("name %q must match %s", name, nameRe)
	}
	fa, err := Analyze(name+".go", code)
	if err != nil {
		rep := &Report{Name: name, State: StateDraft, Issues: []Issue{{Error, "source", err.Error()}}}
		return nil, rep, rep.Err()
	}
	m := Infer(name, fa)
	lock := s.nameLock(name)
	lock.Lock()
	defer lock.Unlock()
	var rep *Report
	for attempt := 0; attempt < 2; attempt++ {
		m.Version = "0.1.0"
		s.mu.Lock()
		if p := s.st.Plugins[name]; p != nil {
			if v, err := parseSemverFull(newestAny(p)); err == nil {
				m.Version = fmt.Sprintf("%d.%d.%d", v.nums[0], v.nums[1], v.nums[2]+1)
			}
		}
		s.mu.Unlock()
		src := &Source{Name: name, Manifest: m, Files: map[string][]byte{name + ".go": code}}
		data, r, err := Build(src, key)
		if err != nil {
			return nil, r, err
		}
		rep = r
		v, err := s.Install(data, by)
		if errors.Is(err, ErrVersionExists) && attempt == 0 {
			continue // another process allocated the same version
		}
		return v, rep, err
	}
	return nil, rep, ErrVersionExists
}
