package pluginpkg

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"eipagent/engine"
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
	ErrNotFound    = errors.New("plugin not installed")
	ErrNotApproved = errors.New("plugin version is not approved")
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
	dir  string
	opts StoreOptions
	mu   sync.Mutex
	st   struct {
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
		out = append(out, *p)
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
	cp := *p
	return &cp, nil
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
		return nil, fmt.Errorf("%s %s is already installed with different contents; bump the version", m.Name, m.Version)
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
		version = newest(p, StateApproved)
		if version == "" {
			if p.Active != "" {
				return nil
			}
			return fmt.Errorf("%s: no approved version to enable", name)
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
	// Re-verify the stored archive: catches tampering at rest.
	pkg, err := s.openLocked(name, version)
	if err != nil || pkg.SHA256 != v.SHA256 {
		if err == nil {
			err = fmt.Errorf("stored package changed since install")
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
		return err
	}
	if rt := s.opts.Runtime; rt != nil {
		if err := rt.Activate(name, v.Source); err != nil {
			// Put the previous source and pin back; the engine kept the
			// previous instance running.
			if hadOld {
				writeFileAtomic(activeFile, oldSrc)
			} else {
				os.Remove(activeFile)
			}
			p.Active = prevVer
			s.writeLockLocked()
			v.State, v.Error = StateFailed, err.Error()
			s.saveLocked()
			s.audit("plugin.enable_failed", map[string]any{"plugin": name, "version": version, "error": err.Error(), "kept": prevVer})
			return err
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
	if v := p.Versions[p.Active]; v != nil {
		v.State = StateApproved
		if newState != "" {
			v.State = newState
		}
	}
	old := p.Active
	p.Active = ""
	if err := s.writeLockLocked(); err != nil {
		return err
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
	p, _ := s.Get(v.Manifest.Name)
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

func semverLess(a, b string) bool {
	va, ea := parseSemver(a)
	vb, eb := parseSemver(b)
	if ea != nil || eb != nil {
		return a < b
	}
	return cmpSemver(va, vb) < 0
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
