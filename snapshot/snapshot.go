// Package snapshot 管理工程文档的不可变快照与未保存缓冲区。
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Document 是某个 URI 在某一版本下的内容。
type Document struct {
	URI     string `json:"uri"`
	Version int64  `json:"version"`
	Content string `json:"content"`
	Saved   bool   `json:"saved"`
}

// Snapshot 是一次查询所绑定的不可变工程视图。
type Snapshot struct {
	ID     string
	Docs   map[string]Document
	opened map[string]struct{}
}

// Store 保存磁盘版本与打开的未保存缓冲区。
type Store struct {
	saved map[string]Document
	open  map[string]Document // 含未保存改动；Version 单调递增
}

func NewStore() *Store {
	return &Store{saved: map[string]Document{}, open: map[string]Document{}}
}

// PutSaved 登记磁盘上的文件（version 为 0 表示已保存基线）。
func (s *Store) PutSaved(uri, content string) {
	s.saved[uri] = Document{URI: uri, Version: 0, Content: content, Saved: true}
}

// Open 打开文档；若已有未保存缓冲则保留它。
func (s *Store) Open(uri, content string) {
	if _, ok := s.open[uri]; ok {
		return
	}
	s.open[uri] = Document{URI: uri, Version: 1, Content: content, Saved: false}
}

// Change 用未保存内容替换当前会话中的文档并递增版本。
func (s *Store) Change(uri, content string) (Document, error) {
	doc, ok := s.open[uri]
	if !ok {
		base := s.saved[uri]
		doc = Document{URI: uri, Version: 0, Content: base.Content}
	}
	doc.Version++
	doc.Content = content
	doc.Saved = false
	s.open[uri] = doc
	return doc, nil
}

// Close 丢弃未保存缓冲，视图回到磁盘版本。
func (s *Store) Close(uri string) {
	delete(s.open, uri)
}

// Save 把缓冲区内容固化为磁盘版本，版本号归零。
func (s *Store) Save(uri string) error {
	doc, ok := s.open[uri]
	if !ok {
		return fmt.Errorf("document %q is not open", uri)
	}
	doc.Saved = true
	doc.Version = 0
	s.saved[uri] = Document{URI: uri, Version: 0, Content: doc.Content, Saved: true}
	s.open[uri] = doc
	return nil
}

// Build 生成当前会话的不可变快照。打开的文件覆盖磁盘版本。
func (s *Store) Build() *Snapshot {
	docs := make(map[string]Document, len(s.saved)+len(s.open))
	for uri, doc := range s.saved {
		docs[uri] = doc
	}
	opened := map[string]struct{}{}
	for uri, doc := range s.open {
		docs[uri] = doc
		opened[uri] = struct{}{}
	}
	uris := make([]string, 0, len(docs))
	for uri := range docs {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	h := sha256.New()
	for _, uri := range uris {
		h.Write([]byte(uri))
		h.Write([]byte{0})
		d := docs[uri]
		fmt.Fprintf(h, "%d:%d:", d.Version, len(d.Content))
		h.Write([]byte(d.Content))
		h.Write([]byte{0})
	}
	return &Snapshot{ID: hex.EncodeToString(h.Sum(nil))[:16], Docs: docs, opened: opened}
}

// Text 返回快照文档内容。
func (snap *Snapshot) Text(uri string) (string, bool) {
	d, ok := snap.Docs[uri]
	return d.Content, ok
}

// Version 返回文档版本；已保存为 0，未保存为正整数。
func (snap *Snapshot) Version(uri string) (int64, bool) {
	d, ok := snap.Docs[uri]
	return d.Version, ok
}

// LineCol 把字节偏移转换为 1 基行列（按 UTF-8 字节，遇换行换行）。
func LineCol(content string, offset int) (line, col int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(content) {
		offset = len(content)
	}
	line, col = 1, 1
	for i, r := range content[:offset] {
		if r == '\n' {
			line++
			col = 1
		} else {
			_ = i
			col++
		}
	}
	return line, col
}
