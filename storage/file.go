package storage

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"

	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/metainfo"
)

type FilePathMaker func(baseDir string, infoHash metainfo.Hash, info *metainfo.Info, finfo *metainfo.FileInfo) string
type FileOption func(*fileClientImpl)

func FileOptionPathMaker(m FilePathMaker) FileOption {
	return func(fci *fileClientImpl) {
		fci.pathMaker = m
	}
}

func FileOptionPathMakerInfohash(fci *fileClientImpl) {
	fci.pathMaker = InfoHashPathMaker
}

func FileOptionPathMakerInfohashV0(fci *fileClientImpl) {
	fci.pathMaker = infoHashPathMakerV0
}

func FileOptionPathMakerFixed(s string) FileOption {
	return func(fci *fileClientImpl) {
		fci.pathMaker = fixedPathMaker(s)
	}
}

// File-based storage for torrents, that isn't yet bound to a particular
// torrent.
type fileClientImpl struct {
	baseDir   string
	pathMaker FilePathMaker
}

func fixedPathMaker(name string) FilePathMaker {
	return func(baseDir string, infoHash metainfo.Hash, info *metainfo.Info, fi *metainfo.FileInfo) string {
		s := filepath.Join(baseDir, name)
		return s
	}
}

func InfoHashPathMaker(baseDir string, infoHash metainfo.Hash, info *metainfo.Info, fi *metainfo.FileInfo) string {
	s := filepath.Join(baseDir, infoHash.String(), filepath.Join(langx.DerefOrZero(fi).Path...))
	return s
}

func infoHashPathMakerV0(baseDir string, infoHash metainfo.Hash, info *metainfo.Info, fi *metainfo.FileInfo) string {
	return filepath.Join(baseDir, infoHash.String(), langx.DerefOrZero(info).Name, filepath.Join(langx.DerefOrZero(fi).Path...))
}

// All Torrent data stored in this baseDir
func NewFile(baseDir string, options ...FileOption) *fileClientImpl {
	return langx.Autoptr(langx.Clone(fileClientImpl{
		baseDir:   baseDir,
		pathMaker: InfoHashPathMaker,
	}, options...))
}

func (me *fileClientImpl) Close() error {
	return nil
}

func (fs *fileClientImpl) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (TorrentImpl, error) {
	upverted := info.UpvertedFiles()
	entries := make([]fileEntry, len(upverted))
	begin := int64(0)
	for i, fi := range upverted {
		path := fs.pathMaker(fs.baseDir, infoHash, info, &fi)
		entries[i] = fileEntry{
			path:   path,
			begin:  begin,
			length: fi.Length,
		}
		begin += fi.Length
	}

	if err := createAllDirectories(entries); err != nil {
		return nil, err
	}

	if err := CreateNativeZeroLengthFiles(fs.baseDir, infoHash, info, fs.pathMaker); err != nil {
		return nil, err
	}

	return &fileTorrentImpl{
		closed:      atomic.Bool{},
		info:        info,
		infoHash:    infoHash,
		pathMaker:   fs.pathMaker,
		files:       entries,
		totalLength: begin,
	}, nil
}

type fileEntry struct {
	path   string
	begin  int64
	length int64
}

func createAllDirectories(entries []fileEntry) error {
	for _, e := range entries {
		if err := os.MkdirAll(filepath.Dir(e.path), 0777); err != nil {
			return err
		}
	}
	return nil
}

type fileTorrentImpl struct {
	closed      atomic.Bool
	info        *metainfo.Info
	infoHash    metainfo.Hash
	pathMaker   FilePathMaker
	files       []fileEntry
	totalLength int64
}

// ReadAt implements TorrentImpl.
func (fts *fileTorrentImpl) ReadAt(p []byte, off int64) (n int, err error) {
	if fts.closed.Load() {
		return 0, ErrClosed()
	}
	return fileTorrentImplIO{fts}.ReadAt(p, off)
}

// WriteAt implements TorrentImpl.
func (fts *fileTorrentImpl) WriteAt(p []byte, off int64) (n int, err error) {
	if fts.closed.Load() {
		return 0, ErrClosed()
	}
	return fileTorrentImplIO{fts}.WriteAt(p, off)
}

func (fs *fileTorrentImpl) Close() error {
	fs.closed.Store(true)
	return nil
}

// Creates natives files for any zero-length file entries in the info. This is
// a helper for file-based storages, which don't address or write to zero-
// length files because they have no corresponding pieces.
func CreateNativeZeroLengthFiles(dir string, infohash metainfo.Hash, info *metainfo.Info, pathMaker FilePathMaker) (err error) {
	for _, fi := range info.UpvertedFiles() {
		if fi.Length != 0 {
			continue
		}

		name := pathMaker(dir, infohash, info, &fi)
		var f io.Closer
		if f, err = os.OpenFile(name, os.O_RDONLY|os.O_CREATE, 0600); err != nil {
			return err
		}

		f.Close()
	}

	return nil
}

type ioreadatclose interface {
	io.ReaderAt
	io.Closer
}

// Exposes file-based storage of a torrent, as one big ReadWriterAt.
type fileTorrentImplIO struct {
	fts *fileTorrentImpl
}

func (t *fileTorrentImplIO) reader(path string) (ioreadatclose, error) {
	if f, err := os.Open(path); err == nil {
		return f, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	return nil, io.ErrUnexpectedEOF
}

// Returns EOF on short or missing file.
func (t *fileTorrentImplIO) readFileAt(fe fileEntry, b []byte, off int64) (n int, err error) {
	f, err := t.reader(fe.path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return io.ReadFull(io.NewSectionReader(f, off, fe.length-off), b)
}

// Only returns EOF at the end of the torrent. Premature EOF is ErrUnexpectedEOF.
func (t fileTorrentImplIO) ReadAt(b []byte, off int64) (n int, err error) {
	if off >= t.fts.totalLength {
		return 0, io.EOF
	}

	// Find the starting file using binary search.
	startFile := sort.Search(len(t.fts.files), func(i int) bool {
		return t.fts.files[i].begin > off
	}) - 1

	for i := startFile; i < len(t.fts.files) && len(b) > 0; i++ {
		fe := t.fts.files[i]
		localOff := off - fe.begin
		requested := min(int64(len(b)), fe.length-localOff)
		var n1 int
		n1, err = t.readFileAt(fe, b[:requested], localOff)
		n += n1
		off += int64(n1)
		b = b[n1:]
		if err != nil {
			return n, err
		}
		if int64(n1) < requested {
			return n, io.ErrUnexpectedEOF
		}
	}
	if len(b) == 0 {
		return n, nil
	}
	return n, io.EOF
}

func (t fileTorrentImplIO) WriteAt(p []byte, off int64) (n int, err error) {
	// Find the starting file using binary search.
	startFile := sort.Search(len(t.fts.files), func(i int) bool {
		return t.fts.files[i].begin > off
	}) - 1

	for i := startFile; i < len(t.fts.files) && len(p) > 0; i++ {
		fe := t.fts.files[i]
		localOff := off - fe.begin
		n1 := min(int64(len(p)), fe.length-localOff)
		var f *os.File
		f, err = os.OpenFile(fe.path, os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			return n, err
		}
		var nw int
		nw, err = f.WriteAt(p[:n1], localOff)
		f.Close()
		if err != nil {
			return n, err
		}
		if int64(nw) < n1 {
			return n, io.ErrShortWrite
		}
		n += nw
		off += int64(nw)
		p = p[nw:]
	}
	return n, nil
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
