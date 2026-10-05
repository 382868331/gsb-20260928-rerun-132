// Package semantic builds symbol identity and lexical resolution on top of the
// immutable syntax objects. Symbols are deliberately separate from syntax
// nodes: a symbol survives re-parses and edits, while ranges belong to a
// specific document version.
package semantic

import "sync"

// Kind classifies a symbol.
type Kind int

const (
	KindUnknown Kind = iota
	KindFunc
	KindVar
)

func (k Kind) String() string {
	switch k {
	case KindFunc:
		return "func"
	case KindVar:
		return "var"
	default:
		return "unknown"
	}
}

// Symbol is the stable identity of one declaration. ID is unique within a
// session; identity keys (global name or lexical path) map onto exactly one
// Symbol for the session lifetime.
type Symbol struct {
	ID   uint64 `json:"id"`
	Kind Kind   `json:"kind"`
	Name string `json:"name"`

	// Module is the exporting module path, or "" for an anonymous-world
	// declaration (a file without a module directive).
	Module string `json:"module,omitempty"`

	// LocalKey is empty for global symbols. For locals it encodes file +
	// lexical nesting path + name, so two same-named locals in different
	// scopes never share identity.
	LocalKey string `json:"localKey,omitempty"`
}

// Registry owns the session-wide symbol table and ID allocation.
type Registry struct {
	mu      sync.Mutex
	nextID  uint64
	globals map[string]*Symbol // name -> stable identity (whole project)
	locals  map[string]*Symbol // localKey -> stable identity
	aliases map[string]bool    // global keys bound by re-export, not declaration
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		globals: make(map[string]*Symbol),
		locals:  make(map[string]*Symbol),
		aliases: make(map[string]bool),
	}
}

// GlobalKey is the registry key of a declaration exported by module mod.
// An empty mod denotes the shared anonymous world (files without a module
// directive); anonymous declarations are project-wide bare names exactly as
// in the pre-module language.
func GlobalKey(mod, name string) string {
	return mod + "\x00" + name
}

// Global returns the stable symbol for a module-qualified declaration,
// creating it on first use.
func (r *Registry) Global(mod, name string, kind Kind) *Symbol {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := GlobalKey(mod, name)
	if r.aliases[key] {
		// A real declaration reclaims the key from a re-export alias (e.g.
		// after a moved declaration is moved back).
		delete(r.globals, key)
		delete(r.aliases, key)
	}
	if s, ok := r.globals[key]; ok {
		if s.Kind == KindUnknown {
			s.Kind = kind
		}
		return s
	}
	r.nextID++
	s := &Symbol{ID: r.nextID, Kind: kind, Name: name, Module: mod}
	r.globals[key] = s
	return s
}

// GlobalExisting returns the module-qualified symbol if registered.
func (r *Registry) GlobalExisting(mod, name string) (*Symbol, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.globals[GlobalKey(mod, name)]
	return s, ok
}

// PruneGlobals drops module-qualified identities whose keys are not in live,
// so declarations removed from the snapshot stop resolving elsewhere.
func (r *Registry) PruneGlobals(live map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.globals {
		if !live[key] {
			delete(r.globals, key)
			delete(r.aliases, key)
		}
	}
}

// Alias binds an existing symbol to an additional module-qualified key. It
// backs re-exports: the alias key and the original key resolve to one shared
// identity, never to a copy.
func (r *Registry) Alias(key string, sym *Symbol) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.globals[key] = sym
	r.aliases[key] = true
}

// Local returns the stable local symbol for a lexical identity key.
func (r *Registry) Local(key, name string, kind Kind) *Symbol {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.locals[key]; ok {
		return s
	}
	r.nextID++
	s := &Symbol{ID: r.nextID, Kind: kind, Name: name, LocalKey: key}
	r.locals[key] = s
	return s
}

// LocalKey builds the identity key of a local declaration.
func LocalKey(uri, scopePath, name string) string {
	return uri + "|" + scopePath + "|" + name
}
