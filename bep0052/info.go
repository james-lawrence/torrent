package bep0052

import (
	"fmt"

	"github.com/james-lawrence/torrent/bencode"
)

// MetaVersion is the value of the info dict's "meta version" key for a
// BEP 52 v2 (or hybrid v1/v2) torrent.
const MetaVersion = 2

// FileTreeEntry is a single file's leaf entry within a BEP 52 "file
// tree": its length and the merkle root of its piece hashes. PiecesRoot
// is the zero value for empty files, per BEP 52.
type FileTreeEntry struct {
	Length     int64
	PiecesRoot Hash
}

// FileTreeNode is either an interior directory (Dir populated) or a file
// leaf (File populated). BEP 52 encodes a file leaf as a dict containing
// a single empty-string key mapping to {length, pieces root}; a directory
// is a dict of child names to further FileTreeNodes.
type FileTreeNode struct {
	Dir  FileTree
	File *FileTreeEntry
}

// FileTree is the recursive directory structure BEP 52 stores in the info
// dict in place of v1's flat file list.
type FileTree map[string]FileTreeNode

func (n FileTreeNode) toAny() any {
	if n.File != nil {
		return map[string]any{
			"": map[string]any{
				"length":      n.File.Length,
				"pieces root": n.File.PiecesRoot[:],
			},
		}
	}

	m := make(map[string]any, len(n.Dir))
	for name, child := range n.Dir {
		m[name] = child.toAny()
	}
	return m
}

func fileTreeNodeFromAny(v any) (FileTreeNode, error) {
	m, ok := v.(map[string]interface{})
	if !ok {
		return FileTreeNode{}, fmt.Errorf("bep0052: file tree node is not a dict: %T", v)
	}

	if leaf, ok := m[""]; ok {
		leafm, ok := leaf.(map[string]interface{})
		if !ok {
			return FileTreeNode{}, fmt.Errorf("bep0052: file leaf is not a dict: %T", leaf)
		}

		entry := &FileTreeEntry{}
		if length, ok := leafm["length"].(int64); ok {
			entry.Length = length
		}
		if root, ok := leafm["pieces root"].(string); ok {
			copy(entry.PiecesRoot[:], root)
		}
		return FileTreeNode{File: entry}, nil
	}

	dir := make(FileTree, len(m))
	for name, child := range m {
		node, err := fileTreeNodeFromAny(child)
		if err != nil {
			return FileTreeNode{}, err
		}
		dir[name] = node
	}
	return FileTreeNode{Dir: dir}, nil
}

// MarshalBencode implements bencode.Marshaler.
func (t FileTree) MarshalBencode() ([]byte, error) {
	m := make(map[string]any, len(t))
	for name, node := range t {
		m[name] = node.toAny()
	}
	return bencode.Marshal(m)
}

// UnmarshalBencode implements bencode.Unmarshaler.
func (t *FileTree) UnmarshalBencode(data []byte) error {
	var raw map[string]interface{}
	if err := bencode.Unmarshal(data, &raw); err != nil {
		return err
	}

	out := make(FileTree, len(raw))
	for name, v := range raw {
		node, err := fileTreeNodeFromAny(v)
		if err != nil {
			return err
		}
		out[name] = node
	}
	*t = out
	return nil
}

// PieceLayers is the top-level "piece layers" dict stored alongside (not
// inside) the info dict: for each file's pieces root, the concatenated
// SHA-256 hashes of that file's pieces.
type PieceLayers map[Hash][]byte

// Set stores hashes for root, concatenating them in order.
func (p PieceLayers) Set(root Hash, hashes []Hash) {
	buf := make([]byte, 0, len(hashes)*HashSize)
	for _, h := range hashes {
		buf = append(buf, h[:]...)
	}
	p[root] = buf
}

// Get returns the per-piece hashes stored for root.
func (p PieceLayers) Get(root Hash) []Hash {
	raw := p[root]
	hashes := make([]Hash, 0, len(raw)/HashSize)
	for i := 0; i+HashSize <= len(raw); i += HashSize {
		var h Hash
		copy(h[:], raw[i:i+HashSize])
		hashes = append(hashes, h)
	}
	return hashes
}

// MarshalBencode implements bencode.Marshaler, encoding piece layers
// keyed by the raw bytes of each pieces root, per BEP 52.
func (p PieceLayers) MarshalBencode() ([]byte, error) {
	m := make(map[string]any, len(p))
	for root, raw := range p {
		m[string(root[:])] = raw
	}
	return bencode.Marshal(m)
}

// UnmarshalBencode implements bencode.Unmarshaler.
func (p *PieceLayers) UnmarshalBencode(data []byte) error {
	var raw map[string]string
	if err := bencode.Unmarshal(data, &raw); err != nil {
		return err
	}

	out := make(PieceLayers, len(raw))
	for k, v := range raw {
		if len(k) != HashSize {
			return fmt.Errorf("bep0052: piece layers key has bad length: %d", len(k))
		}
		var root Hash
		copy(root[:], k)
		out[root] = []byte(v)
	}
	*p = out
	return nil
}
