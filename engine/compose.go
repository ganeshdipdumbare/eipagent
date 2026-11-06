package engine

import (
	"fmt"
	"sort"
	"strings"
)

// Options describes an application: the modules it can use (Registry),
// which ones run and with what config (Specs), and the admin Policy.
type Options struct {
	Registry []Module
	Specs    []ModuleSpec
	Policy   *Policy // nil = SecurePolicy()
	Logger   func(scope, msg string)
}

// Plan is a validated, immutable composition. Compose builds it without
// running any module code; Start runs it.
type Plan struct {
	pol    *Policy
	logger func(scope, msg string)
	nodes  []node // in start order
}

type node struct {
	required bool   // a policy Require row
	name     string // instance name
	mod      Module
	spec     ModuleSpec
	granted  []string
	locked   bool
	priority int // profile position; ties in ordering use it
}

// ModuleInfo describes one module of a plan.
type ModuleInfo struct {
	Name        string // instance name
	Module      string // the module it was built from
	Order       int    // start position, from 0
	Requires    []string
	Provides    []string
	Declared    []string // Permissions as the module declared them
	Permissions []string // granted by the policy
	Secrets     []string
	Locked      bool
	Auto        bool // added because a requirement needed it
}

// Compose validates opts and returns the plan. It fails with a named
// error for every condition that would make a run unsafe or impossible:
// unknown or denied modules, duplicate providers, unmet requirements,
// unknown capabilities and dependency cycles.
func Compose(opts Options) (*Plan, error) {
	pol := opts.Policy
	if pol == nil {
		pol = SecurePolicy()
	}

	// 1. The registry: every candidate must be well formed.
	reg := map[string]Module{}
	for _, m := range opts.Registry {
		if !validName(m.Name) {
			return nil, fmt.Errorf("module name %q: invalid (letters, digits, '_' and '-' only)", m.Name)
		}
		if _, dup := reg[m.Name]; dup {
			return nil, fmt.Errorf("module %s: registered twice", m.Name)
		}
		if m.Apply == nil {
			return nil, fmt.Errorf("module %s: Apply is nil", m.Name)
		}
		if err := checkPermissions(m.Name, m.Permissions); err != nil {
			return nil, err
		}
		for _, p := range m.Provides {
			if p == "" || strings.Count(p, "*") > 1 || (strings.Contains(p, "*") && !strings.HasSuffix(p, "*")) {
				return nil, fmt.Errorf("module %s: provide %q: a pattern may only end in \"*\"", m.Name, p)
			}
		}
		reg[m.Name] = m
	}

	// 2. The rows: policy requirements first, then the profile. A required
	// row replaces a profile row with the same name.
	type row struct {
		spec ModuleSpec
		pos  int
	}
	var rows []row
	seen := map[string]bool{}
	for _, r := range pol.Require {
		if seen[r.Name()] {
			return nil, fmt.Errorf("policy requires module %s twice", r.Name())
		}
		seen[r.Name()] = true
		rows = append(rows, row{spec: r, pos: len(rows)})
	}
	for _, s := range opts.Specs {
		if seen[s.Name()] {
			if contains(requiredNames(pol), s.Name()) {
				continue // the policy's row wins
			}
			// The same module listed again is configuration, not a second
			// instance: merge it so option order never changes the result.
			merged := false
			for i := range rows {
				if rows[i].spec.Name() == s.Name() && rows[i].spec.Use == s.Use {
					rows[i].spec.Config = mergeConfig(rows[i].spec.Config, s.Config)
					merged = true
					break
				}
			}
			if merged {
				continue
			}
			return nil, fmt.Errorf("module %s listed twice", s.Name())
		}
		seen[s.Name()] = true
		rows = append(rows, row{spec: s, pos: len(rows)})
	}

	// 3. Admission: known, not denied, not disabled when locked.
	var nodes []node
	for _, r := range rows {
		isRequired := contains(requiredNames(pol), r.spec.Name())
		name := r.spec.Use
		inst := r.spec.Name()
		if !validName(name) || !validName(inst) {
			return nil, fmt.Errorf("module %q: invalid name", name)
		}
		mod, ok := reg[name]
		if !ok {
			return nil, fmt.Errorf("unknown module %q: not in the registry (import its package and register it)", name)
		}
		if r.spec.Disabled {
			if pol.locked(name) {
				return nil, fmt.Errorf("module %s is locked by policy and cannot be disabled", name)
			}
			continue
		}
		if !pol.allowed(name) {
			return nil, fmt.Errorf("module %s is not allowed by policy", name)
		}
		r.spec.Config = mergeConfig(mod.Config, r.spec.Config)
		if pol.Rules != nil && name == "policy" {
			cfg := map[string]any{}
			for k, v := range r.spec.Config {
				cfg[k] = v
			}
			cfg["rules"] = pol.Rules
			r.spec.Config = cfg
		}
		nodes = append(nodes, node{
			required: isRequired,
			name:     inst,
			mod:      mod,
			spec:     r.spec,
			granted:  pol.granted(name, mod.Permissions),
			locked:   pol.locked(name),
			priority: r.pos,
		})
	}

	// 4. Providers: no two enabled modules may claim the same name.
	for i := range nodes {
		for j := i + 1; j < len(nodes); j++ {
			for _, a := range nodes[i].mod.Provides {
				for _, b := range nodes[j].mod.Provides {
					if overlaps(a, b) {
						return nil, fmt.Errorf("modules %s and %s both provide %q; keep one or rename its service",
							nodes[i].name, nodes[j].name, a)
					}
				}
			}
		}
	}

	// 5. Requirements: every required service has a provider; the provider
	// must start first. Build the edges as we go.
	index := map[string]int{}
	for i, n := range nodes {
		index[n.name] = i
	}
	deps := make([][]int, len(nodes)) // deps[i]: nodes that must start before i
	for i, n := range nodes {
		for _, req := range n.mod.Requires {
			optional := strings.HasPrefix(req, "?")
			svc := strings.TrimPrefix(req, "?")
			if svc == "" {
				return nil, fmt.Errorf("module %s: empty requirement", n.name)
			}
			found := false
			for j, p := range nodes {
				if j == i {
					continue
				}
				if provides(p.mod, svc) {
					found = true
					deps[i] = appendUnique(deps[i], j)
				}
			}
			if !found && !optional {
				return nil, unmet(n.name, svc, reg)
			}
		}
	}

	// 6. Order: Kahn's algorithm. Among ready modules take the lowest
	// profile position, then the name, so the order is deterministic.
	// Policy-required rows start in the order the policy lists them, so an
	// audit row listed before the rest sees everything that starts after it.
	prevRequired := -1
	for i, n := range nodes {
		if !n.required {
			continue
		}
		if prevRequired >= 0 {
			deps[i] = appendUnique(deps[i], prevRequired)
		}
		prevRequired = i
	}

	order, err := topo(nodes, deps)
	if err != nil {
		return nil, err
	}
	planned := make([]node, 0, len(order))
	for _, i := range order {
		planned = append(planned, nodes[i])
	}
	return &Plan{pol: pol, logger: opts.Logger, nodes: planned}, nil
}

// mergeConfig returns base with over applied on top (over wins).
func mergeConfig(base, over map[string]any) map[string]any {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func requiredNames(p *Policy) []string {
	var out []string
	for _, r := range p.Require {
		out = append(out, r.Use)
	}
	return out
}

// provides reports whether module m provides service svc. A need ending
// in "*" is satisfied by any provide entry under that prefix.
func provides(m Module, svc string) bool {
	for _, p := range m.Provides {
		if strings.HasSuffix(svc, "*") {
			prefix := strings.TrimSuffix(svc, "*")
			if strings.HasPrefix(strings.TrimSuffix(p, "*"), prefix) {
				return true
			}
			continue
		}
		if providesMatch(p, svc) {
			return true
		}
	}
	return false
}

func appendUnique(xs []int, x int) []int {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// unmet explains a missing requirement and names the registry modules that
// would supply it.
func unmet(module, svc string, reg map[string]Module) error {
	var hint []string
	for name, m := range reg {
		if name != module && provides(m, svc) {
			hint = append(hint, name)
		}
	}
	sort.Strings(hint)
	msg := fmt.Sprintf("module %s requires %q, which no enabled module provides", module, svc)
	if len(hint) > 0 {
		msg += fmt.Sprintf(" (add one of: %s)", strings.Join(hint, ", "))
	}
	return fmt.Errorf("%s", msg)
}

// topo orders nodes so each comes after its dependencies. Ready nodes are
// taken in profile position, then name, which keeps the order stable.
func topo(nodes []node, deps [][]int) ([]int, error) {
	n := len(nodes)
	indeg := make([]int, n)
	users := make([][]int, n)
	for i, ds := range deps {
		for _, d := range ds {
			indeg[i]++
			users[d] = append(users[d], i)
		}
	}
	less := func(a, b int) bool {
		if nodes[a].priority != nodes[b].priority {
			return nodes[a].priority < nodes[b].priority
		}
		return nodes[a].name < nodes[b].name
	}
	var ready, out []int
	for i := range nodes {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	for len(ready) > 0 {
		sort.Slice(ready, func(x, y int) bool { return less(ready[x], ready[y]) })
		cur := ready[0]
		ready = ready[1:]
		out = append(out, cur)
		for _, u := range users[cur] {
			indeg[u]--
			if indeg[u] == 0 {
				ready = append(ready, u)
			}
		}
	}
	if len(out) != n {
		var stuck []string
		for i := range nodes {
			if indeg[i] > 0 {
				stuck = append(stuck, nodes[i].name)
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("dependency cycle among modules: %s", strings.Join(stuck, " -> "))
	}
	return out, nil
}

// Order returns the module names in start order.
func (p *Plan) Order() []string {
	out := make([]string, len(p.nodes))
	for i, n := range p.nodes {
		out[i] = n.name
	}
	return out
}

// Describe returns one ModuleInfo per planned module, in start order.
func (p *Plan) Describe() []ModuleInfo {
	out := make([]ModuleInfo, len(p.nodes))
	for i, n := range p.nodes {
		out[i] = ModuleInfo{
			Name:        n.name,
			Module:      n.mod.Name,
			Order:       i,
			Requires:    append([]string(nil), n.mod.Requires...),
			Provides:    append([]string(nil), n.mod.Provides...),
			Declared:    append([]string(nil), n.mod.Permissions...),
			Permissions: append([]string(nil), n.granted...),
			Secrets:     append([]string(nil), n.mod.Secrets...),
			Locked:      n.locked,
			Auto:        n.spec.Auto,
		}
	}
	return out
}
