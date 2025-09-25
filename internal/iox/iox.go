package iox

import (
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/james-lawrence/torrent/internal/errorsx"
)

// Error return just the error from an IO call ignoring the number of bytes.
func Error(_ int64, err error) error {
	return err
}

type errReader struct {
	error
}

func (t errReader) Read([]byte) (int, error) {
	return 0, t
}

func (t errReader) Seek(offset int64, whence int) (int64, error) {
	return 0, t
}

func (t errReader) Close() error {
	return t
}

// ErrReader returns an io.Reader that returns the provided error.
func ErrReader(err error) io.ReadSeekCloser {
	return errReader{err}
}

// Rewind an io.Seeker
func Rewind(o io.Seeker) error {
	_, err := o.Seek(0, io.SeekStart)
	return err
}

type writeNopCloser struct {
	io.Writer
}

func (writeNopCloser) Close() error { return nil }

// WriteNopCloser returns a WriteCloser with a no-op Close method wrapping
// the provided Writer w.
func WriteNopCloser(w io.Writer) io.WriteCloser {
	return writeNopCloser{w}
}

// Copy a file to another path
func Copy(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()

	i, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, i.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}

	return out.Close()
}

func MaybeClose(c io.Closer) error {
	if c == nil || c == (*os.File)(nil) {
		return nil
	}

	return c.Close()
}

type zreader struct{}

func (z *zreader) Read(b []byte) (n int, err error) {
	for i := range b {
		b[i] = 0
	}

	return len(b), nil
}

func Zero() io.Reader {
	return &zreader{}
}

func String(r io.Reader) string {
	defer func() {
		if x, ok := r.(io.Seeker); ok {
			_ = Rewind(x)
		}
	}()

	raw, _ := io.ReadAll(r)

	return string(raw)
}

type readCompositeCloser struct {
	io.Reader
	closefn []func() error
}

func (t readCompositeCloser) Close() (err error) {
	for _, fn := range t.closefn {
		err = errorsx.Compact(err, fn())
	}
	return err
}

// WriteNopCloser returns a WriteCloser with a no-op Close method wrapping
// the provided Writer w.
func ReaderCompositeCloser(w io.Reader, closers ...func() error) io.ReadCloser {
	return readCompositeCloser{Reader: w, closefn: closers}
}

func CloseReadSeeker(d io.ReadSeeker, c io.Closer) closeableReadSeeker {
	return closeableReadSeeker{ReadSeeker: d, Closer: c}
}

type closeableReadSeeker struct {
	io.ReadSeeker
	io.Closer
}

func (t closeableReadSeeker) Close() error {
	return t.Closer.Close()
}

// deprecated: this is a silly function do not use.
func CopyExact(dest interface{}, src interface{}) {
	dV := reflect.ValueOf(dest)
	sV := reflect.ValueOf(src)
	if dV.Kind() == reflect.Ptr {
		dV = dV.Elem()
	}
	if dV.Kind() == reflect.Array && !dV.CanAddr() {
		panic(fmt.Sprintf("dest not addressable: %T", dest))
	}
	if sV.Kind() == reflect.Ptr {
		sV = sV.Elem()
	}
	if sV.Kind() == reflect.String {
		sV = sV.Convert(reflect.SliceOf(dV.Type().Elem()))
	}
	if !sV.IsValid() {
		panic("invalid source, probably nil")
	}
	if dV.Len() != sV.Len() {
		panic(fmt.Sprintf("dest len (%d) != src len (%d)", dV.Len(), sV.Len()))
	}
	if dV.Len() != reflect.Copy(dV, sV) {
		panic("dammit")
	}
}
