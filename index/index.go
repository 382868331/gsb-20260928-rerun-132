// Package index 维护快照之上的增量符号索引。
// 变更文件才重新语法分析；引用按全局依赖集合做局部重绑定。
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"nav/analyze"
	"nav/snapshot"
	"nav/symbol"
)

type globalOwner struct {
	uri  string
	kind symbol.Kind
}

// Project 是可跨快照复用的索引。缓存只依赖单文件内容。
type Project struct {
	cache   map[string]fileCache
	refs    map[symbol.ID][]symbol.Ref
	symbols map[symbol.ID]symbol.Symbol
	globals map[string]globalOwner
	snapID  string
}

type fileCache struct {
	hash   string
	result *analyze.FileResult
}

func New() *Project {
	return &Project{
		cache:   map[string]fileCache{},
		refs:    map[symbol.ID][]symbol.Ref{},
		symbols: map[symbol.ID]symbol.Symbol{},
		globals: map[string]globalOwner{},
	}
}

// Update 依据新快照做增量更新，返回本次受影响的文件集合。
func (p *Project) Update(snap *snapshot.Snapshot) (affected map[string]struct{}) {
	affected = map[string]struct{}{}

	// 1. 删除已不存在的文件，找出内容变化需重新分析的文件。
	for uri := range p.cache {
		if _, ok := snap.Docs[uri]; !ok {
			delete(p.cache, uri)
			affected[uri] = struct{}{}
		}
	}
	changed := map[string]struct{}{}
	for uri, doc := range snap.Docs {
		h := contentHash(doc.Content)
		c, ok := p.cache[uri]
		if !ok || c.hash != h {
			res, _ := analyze.Analyze(uri, doc.Content)
			p.cache[uri] = fileCache{hash: h, result: res}
			changed[uri] = struct{}{}
			affected[uri] = struct{}{}
		}
	}

	oldOwners := p.globals
	newOwners := buildGlobalTable(p.cache)

	// 2. 找出定义归属发生变化的全局名，仅这些名字的依赖文件需要重绑定。
	dirtyNames := map[string]struct{}{}
	for name, old := range oldOwners {
		if nw, ok := newOwners[name]; !ok || nw != old {
			dirtyNames[name] = struct{}{}
		}
	}
	for name, nw := range newOwners {
		if old, ok := oldOwners[name]; !ok || old != nw {
			dirtyNames[name] = struct{}{}
		}
	}
	for uri, c := range p.cache {
		if _, selfChanged := changed[uri]; selfChanged {
			continue
		}
		for name := range c.result.GlobalUses {
			if _, dirty := dirtyNames[name]; dirty {
				affected[uri] = struct{}{}
				break
			}
		}
	}

	// 3. 重建查询表（遍历缓存结果；未受影响文件的符号身份不变）。
	p.globals = newOwners
	p.symbols = map[symbol.ID]symbol.Symbol{}
	p.refs = map[symbol.ID][]symbol.Ref{}
	uris := make([]string, 0, len(p.cache))
	for uri := range p.cache {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		c := p.cache[uri]
		for _, sym := range c.result.Symbols {
			// 跨文件重复全局声明时，只保留全局表归属者。
			if sym.ID.Global {
				owner, ok := newOwners[sym.Name]
				if !ok || owner.uri != uri {
					continue
				}
			}
			p.symbols[sym.ID] = sym
		}
		for _, rf := range c.result.Refs {
			id := rf.Def
			if !rf.Resolved {
				_, ok := newOwners[rf.GlobalName]
				if !ok {
					continue // 未解析名字：不计入任何符号的引用
				}
				id = symbol.GlobalID(rf.GlobalName)
				rf.Def = id
				rf.Resolved = true
			}
			p.refs[id] = append(p.refs[id], rf)
		}
	}

	// 4. 把每个声明自身作为定义引用纳入引用列表。
	for _, sym := range p.symbols {
		rf := symbol.Ref{
			File: sym.File, Start: sym.DefStart, End: sym.DefEnd,
			Def: sym.ID, Resolved: true,
		}
		p.refs[sym.ID] = append([]symbol.Ref{rf}, p.refs[sym.ID]...)
	}
	p.snapID = snap.ID
	return affected
}

// SnapshotID 返回当前索引绑定的快照。
func (p *Project) SnapshotID() string { return p.snapID }

func buildGlobalTable(cache map[string]fileCache) map[string]globalOwner {
	uris := make([]string, 0, len(cache))
	for uri := range cache {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	owners := map[string]globalOwner{}
	for _, uri := range uris {
		for _, g := range cache[uri].result.Globals {
			if _, exists := owners[g.Name]; !exists {
				owners[g.Name] = globalOwner{uri: uri, kind: g.Kind}
			}
		}
	}
	return owners
}

// Symbol 按身份取回符号当前版本信息。
func (p *Project) Symbol(id symbol.ID) (symbol.Symbol, bool) {
	s, ok := p.symbols[id]
	return s, ok
}

// DefinitionAt 把位置解析到其引用的定义符号；位置落在声明名上时返回该声明。
func (p *Project) DefinitionAt(uri string, offset int) (symbol.Symbol, []symbol.Ref, bool) {
	for _, sym := range p.symbols {
		if sym.File == uri && offset >= sym.DefStart && offset < sym.DefEnd {
			return sym, p.refs[sym.ID], true
		}
	}
	for id, refs := range p.refs {
		for _, rf := range refs {
			if rf.File == uri && offset >= rf.Start && offset < rf.End {
				if sym, ok := p.symbols[id]; ok {
					return sym, refs, true
				}
			}
		}
	}
	return symbol.Symbol{}, nil, false
}

// References 返回符号的全部引用（第一个为定义本身）。
func (p *Project) References(id symbol.ID) ([]symbol.Symbol, []symbol.Ref) {
	sym, ok := p.symbols[id]
	if !ok {
		return nil, nil
	}
	return []symbol.Symbol{sym}, p.refs[id]
}

// Symbols 列出当前全部符号（主要用于调试与补全功能预留）。
func (p *Project) Symbols() []symbol.Symbol {
	out := make([]symbol.Symbol, 0, len(p.symbols))
	for _, s := range p.symbols {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out
}

func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
