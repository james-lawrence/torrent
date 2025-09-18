package metainfo

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/langx"
)

type Option func(*Info)

func OptionPieceLength(n int64) Option {
	return func(i *Info) {
		i.PieceLength = n
	}
}

func OptionDisplayName(s string) Option {
	return func(i *Info) {
		i.Name = s
	}
}

// Creates a new Info from the given reader.
func NewFromReader(src io.Reader, options ...Option) (info *Info, err error) {
	info = langx.Autoptr(langx.Clone(Info{
		PieceLength: bytesx.MiB,
	}, options...))

	var wrapped io.Reader = src
	var digest io.Writer
	if info.Name == "" {
		md5Hasher := md5.New()
		digest = md5Hasher
		wrapped = io.TeeReader(wrapped, digest)
	}
	length := readlength(0)
	wrapped = io.TeeReader(wrapped, &length)
	if info.Pieces, err = ComputePieces(wrapped, info.PieceLength, 0); err != nil {
		return nil, err
	}

	info.Length = int64(length)
	if info.Name == "" {
		info.Name = hex.EncodeToString(digest.(hash.Hash).Sum(nil))
	}

	return info, nil
}

func NewInfo(options ...Option) *Info {
	return langx.Autoptr(langx.Clone(Info{
		PieceLength: bytesx.MiB,
	}, options...))
}

func NewInfoFromReader(r io.Reader, options ...Option) (_ *Info, err error) {
	var info Info
	if err = bencode.NewDecoder(r).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Creates a new Info from the given file path.
func NewFromPath(root string, options ...Option) (info *Info, err error) {
	info = langx.Autoptr(langx.Clone(Info{
		Name:        filepath.Base(root),
		PieceLength: bytesx.MiB,
	}, options...))

	type tempFile struct {
		rel    string
		length int64
	}
	var temp []tempFile

	err = filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if fi.IsDir() {
			// Directories are implicit in torrent files.
			return nil
		} else if path == root {
			// The root is a file.
			info.Length = fi.Size()
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("error getting relative path: %s", err)
		}
		temp = append(temp, tempFile{
			rel:    relPath,
			length: fi.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(temp, func(i, j int) bool {
		return temp[i].rel < temp[j].rel
	})
	for _, t := range temp {
		info.Files = append(info.Files, FileInfo{
			Path:   strings.Split(t.rel, string(filepath.Separator)),
			Length: t.length,
		})
	}

	err = info.GeneratePieces(func(fi FileInfo) (io.ReadCloser, error) {
		return os.Open(filepath.Join(root, strings.Join(fi.Path, string(filepath.Separator))))
	})
	if err != nil {
		return nil, fmt.Errorf("error generating pieces: %s", err)
	}

	return info, err
}

// Computes the pieces from the given reader and piece length, with optional preallocation capacity.
func ComputePieces(src io.Reader, pieceLength int64, capacity int) (pieces []byte, err error) {
	if pieceLength == 0 {
		return nil, errors.New("piece length must be non-zero")
	}
	if capacity > 0 {
		pieces = make([]byte, 0, capacity)
	}
	hasher := sha1.New()
	for {
		hasher.Reset()
		wn, err := io.CopyN(hasher, src, pieceLength)
		if err == io.EOF {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		if wn == 0 {
			break
		}
		pieces = hasher.Sum(pieces)
		if wn < pieceLength {
			break
		}
	}

	return pieces, nil
}

// Info dictionary.
type Info struct {
	PieceLength  int64      `bencode:"piece length"`
	Pieces       []byte     `bencode:"pieces"`
	Name         string     `bencode:"name"`
	Length       int64      `bencode:"length,omitempty"`
	Private      *bool      `bencode:"private,omitempty"` // pointer to handle backwards compatibility
	Source       string     `bencode:"source,omitempty"`
	Files        []FileInfo `bencode:"files,omitempty"`
	cachedLength int64      // used to cache the total length of the torrent
}

// Concatenates all the files in the torrent into w. open is a function that
// gets at the contents of the given file.
func (info *Info) writeFiles(w io.Writer, open func(fi FileInfo) (io.ReadCloser, error)) error {
	for _, fi := range info.UpvertedFiles() {
		r, err := open(fi)
		if err != nil {
			return fmt.Errorf("error opening %v: %s", fi, err)
		}
		wn, err := io.CopyN(w, r, fi.Length)
		r.Close()
		if wn != fi.Length {
			return fmt.Errorf("error copying %v: %s", fi, err)
		}
	}
	return nil
}

// Sets Pieces (the block of piece hashes in the Info) by using the passed
// function to get at the torrent data.
func (info *Info) GeneratePieces(open func(fi FileInfo) (io.ReadCloser, error)) (err error) {
	total := info.TotalLength()
	numPieces := (total + info.PieceLength - 1) / info.PieceLength
	capacity := int(numPieces) * sha1.Size
	pr, pw := io.Pipe()
	go func() {
		err := info.writeFiles(pw, open)
		pw.CloseWithError(err)
	}()
	defer pr.Close()
	info.Pieces, err = ComputePieces(pr, info.PieceLength, capacity)
	return err
}

func (info *Info) TotalLength() (ret int64) {
	if cached := atomic.LoadInt64(&info.cachedLength); cached > 0 {
		return cached
	}
	defer func() {
		atomic.StoreInt64(&info.cachedLength, ret)
	}()

	if !info.IsDir() {
		return info.Length
	}

	for _, fi := range info.Files {
		ret += fi.Length
	}

	return ret
}

func (info *Info) NumPieces() uint64 {
	return uint64(len(info.Pieces) / 20)
}

func (info *Info) IsDir() bool {
	return len(info.Files) != 0
}

// The files field, converted up from the old single-file in the parent info
// dict if necessary. This is a helper to avoid having to conditionally handle
// single and multi-file torrent infos.
func (info *Info) UpvertedFiles() []FileInfo {
	if len(info.Files) == 0 {
		return []FileInfo{{
			Length: info.Length,
			// Callers should determine that Info.Name is the basename, and
			// thus a regular file.
			Path: nil,
		}}
	}
	return info.Files
}

func (info *Info) Piece(index int) Piece {
	return Piece{info, pieceIndex(index)}
}

func (info *Info) OffsetToIndex(offset int64) int64 {
	if info.PieceLength == 0 {
		return 0
	}

	return min(offset/info.PieceLength, int64(info.NumPieces()-1))
}

func (info *Info) OffsetToLength(offset int64) (length int64) {
	if info.PieceLength == 0 {
		return 0
	}

	index := min(offset/info.PieceLength, int64(info.NumPieces()))
	if index == int64(info.NumPieces()) {
		return info.TotalLength() % info.PieceLength
	}

	return info.PieceLength
}

func (info *Info) Hashes() (ret [][]byte) {
	for i := 0; i < len(info.Pieces); i += sha1.Size {
		ret = append(ret, info.Pieces[i:i+sha1.Size])
	}

	return ret
}

type readlength uint64

func (t *readlength) Write(b []byte) (int, error) {
	bn := len(b)
	atomic.AddUint64((*uint64)(t), uint64(bn))
	return bn, nil
}

func Files(info *Info) iter.Seq[File] {
	return func(yield func(File) bool) {
		if !info.IsDir() {
			yield(File{
				Path:   info.Name,
				Offset: 0,
				Length: uint64(info.TotalLength()),
			})
			return
		}

		for _, fd := range info.Files {
			c := File{
				Path:   fd.DisplayPath(info),
				Offset: uint64(fd.Offset(info)),
				Length: uint64(fd.Length),
			}

			if !yield(c) {
				return
			}
		}
	}
}
