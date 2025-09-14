package torrent

import (
	"io"
	"log"
)

type logging interface {
	Println(v ...any)
	Printf(format string, v ...any)
	Print(v ...any)
}

type discard struct{}

func (discard) Output(int, string) error {
	return nil
}

// Println replicates the behaviour of the standard logger.
func (t discard) Println(v ...any) {
}

func (t discard) Printf(format string, v ...any) {
}

func (t discard) Print(v ...any) {

}

type logoutput interface {
	Writer() io.Writer
}

// if possible use the provided logger as a base, otherwise fallback to the default
func newlogger(l logging, prefix string, flags int) *log.Logger {
	if l, ok := l.(logoutput); ok {
		return log.New(l.Writer(), prefix, flags)
	}

	return log.New(io.Discard, prefix, log.Flags())
}

func LogDiscard() discard {
	return discard{}
}
