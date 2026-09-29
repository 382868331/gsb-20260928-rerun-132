// Package service 把快照、语法、符号身份与增量索引编排成导航服务。
package service

import (
	"nav/index"
	"nav/snapshot"
	"nav/symbol"
)

type Location struct {
	URI     string `json:"uri"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Text    string `json:"text"`
	Version int64  `json:"version"`
}

type SymbolInfo struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Container string   `json:"container"`
	Def       Location `json:"definition"`
}

type ResultBase struct {
	SnapshotID string           `json:"snapshot_id"`
	Versions   map[string]int64 `json:"versions"` // 结果涉及文档的版本
}

type DefinitionResult struct {
	ResultBase
	Symbol SymbolInfo `json:"symbol"`
}

type ReferencesResult struct {
	ResultBase
	Symbol   SymbolInfo `json:"symbol"`
	Refs     []Location `json:"references"`
	Affected []string   `json:"affected_files,omitempty"` // 本次改动重算的文件
}

type ChangeResult struct {
	ResultBase
	URI      string   `json:"uri"`
	Version  int64    `json:"version"`
	Affected []string `json:"affected_files"`
}

// Service 是会话状态：磁盘文件 + 未保存缓冲 + 增量索引。
type Service struct {
	store *snapshot.Store
	idx   *index.Project
	snap  *snapshot.Snapshot
}

func New() *Service {
	s := &Service{store: snapshot.NewStore(), idx: index.New()}
	s.refresh()
	return s
}

func (s *Service) refresh() []string {
	s.snap = s.store.Build()
	affected := s.idx.Update(s.snap)
	out := make([]string, 0, len(affected))
	for uri := range affected {
		out = append(out, uri)
	}
	sortStrings(out)
	return out
}

func (s *Service) PutSaved(uri, content string) { s.store.PutSaved(uri, content); s.refresh() }
func (s *Service) Open(uri, content string)     { s.store.Open(uri, content); s.refresh() }
func (s *Service) Close(uri string)             { s.store.Close(uri); s.refresh() }
func (s *Service) Save(uri string)              { _ = s.store.Save(uri); s.refresh() }

func (s *Service) Change(uri, content string) ChangeResult {
	doc, _ := s.store.Change(uri, content)
	affected := s.refresh()
	return ChangeResult{
		ResultBase: s.base(nil),
		URI:        uri, Version: doc.Version, Affected: affected,
	}
}

func (s *Service) Definition(uri string, offset int) (DefinitionResult, bool) {
	sym, _, ok := s.idx.DefinitionAt(uri, offset)
	if !ok {
		return DefinitionResult{ResultBase: s.base(nil)}, false
	}
	res := DefinitionResult{ResultBase: s.base([]string{sym.File}), Symbol: s.symbolInfo(sym)}
	return res, true
}

func (s *Service) References(uri string, offset int) (ReferencesResult, bool) {
	sym, refs, ok := s.idx.DefinitionAt(uri, offset)
	if !ok {
		return ReferencesResult{ResultBase: s.base(nil)}, false
	}
	files := []string{sym.File}
	locs := make([]Location, 0, len(refs))
	for _, rf := range refs {
		locs = append(locs, s.location(rf))
		files = append(files, rf.File)
	}
	return ReferencesResult{
		ResultBase: s.base(files), Symbol: s.symbolInfo(sym), Refs: locs,
	}, true
}

func (s *Service) symbolInfo(sym symbol.Symbol) SymbolInfo {
	return SymbolInfo{
		ID: sym.ID.String(), Kind: sym.Kind.String(), Name: sym.Name,
		Container: sym.Container,
		Def:       s.location(symbol.Ref{File: sym.File, Start: sym.DefStart, End: sym.DefEnd}),
	}
}

func (s *Service) location(rf symbol.Ref) Location {
	content, _ := s.snap.Text(rf.File)
	line, col := snapshot.LineCol(content, rf.Start)
	txt, _ := (syntaxRange(rf)).Text(content)
	ver, _ := s.snap.Version(rf.File)
	return Location{
		URI: rf.File, Start: rf.Start, End: rf.End, Line: line, Column: col,
		Text: txt, Version: ver,
	}
}

func (s *Service) base(files []string) ResultBase {
	versions := map[string]int64{}
	for _, f := range files {
		if v, ok := s.snap.Version(f); ok {
			versions[f] = v
		}
	}
	return ResultBase{SnapshotID: s.snap.ID, Versions: versions}
}

func sortStrings(x []string) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j-1] > x[j]; j-- {
			x[j-1], x[j] = x[j], x[j-1]
		}
	}
}
