package backoffx

import (
	"crypto/md5"
	"encoding/binary"
	"iter"
	"math"
	"math/bits"
	"math/rand"
	"time"

	"github.com/james-lawrence/torrent/internal/timex"
)

// Strategy strategy to compute how long to wait before retrying message.
type Strategy interface {
	Backoff(attempt int) time.Duration
}

// Option consumes a strategy and returns a new strategy.
type Option func(Strategy) Strategy

// Maximum sets an upper bound for the strategy.
func Maximum(d time.Duration) Option {
	return func(s Strategy) Strategy {
		return StrategyFunc(func(attempt int) time.Duration {
			if x := s.Backoff(attempt); x < d {
				return x
			}

			return d
		})
	}
}

// Minimum sets a lower bound for the strategy.
func Minimum(d time.Duration) Option {
	return func(s Strategy) Strategy {
		return StrategyFunc(func(attempt int) time.Duration {
			if x := s.Backoff(attempt); x > d {
				return x
			}

			return d
		})
	}
}

// Jitter set a jitter frame for the strategy.
func Jitter(multiplier float64) Option {
	return func(s Strategy) Strategy {
		return StrategyFunc(func(attempt int) time.Duration {
			x := s.Backoff(attempt)
			if x == math.MaxInt64 {
				return x
			}

			d := math.Floor(float64(x) * multiplier)
			return timex.DurationMax(
				x,
				x+time.Duration(rand.Intn(int(d))),
			)
		})
	}
}

// New backoff
func New(s Strategy, options ...Option) Strategy {
	for _, opt := range options {
		s = opt(s)
	}
	return s
}

// StrategyFunc convience helper to convert a pure function into a backoff strategy.
type StrategyFunc func(attempt int) time.Duration

// Backoff implements Strategy
func (t StrategyFunc) Backoff(attempt int) time.Duration {
	return t(attempt)
}

// Constant always returns the provided duration regardless of the attempt.
func Constant(d time.Duration) Strategy {
	return StrategyFunc(func(attempt int) time.Duration {
		return d
	})
}

type exponential struct {
	scale time.Duration
}

func (t *exponential) Backoff(attempt int) (exp time.Duration) {
	// if the exponential wraps around fall back to using maximum.
	exp = time.Duration(1 << uint64(attempt))
	if exp <= 0 {
		return time.Duration(math.MaxInt64)
	}

	hi, lo := bits.Mul64(uint64(exp), uint64(t.scale))

	// check if we overflowed into hi bits, or if the low bits
	// are negative.
	if hi != 0 || (lo)&(1<<63) == (1<<63) {
		return time.Duration(math.MaxInt64)
	}

	return time.Duration(lo)
}

// Exponential implements expoential backoff.
func Exponential(scale time.Duration) Strategy {
	if scale == 0 {
		panic("exponential backoff can't be scaled by 0")
	}

	return &exponential{
		scale: scale,
	}
}

// Cycle an explicit set of delays to use. if the attempt is larger than
// the number of values it restarts at the first delay.
func Cycle(delays ...time.Duration) Strategy {
	return explicit{delays: delays}
}

type explicit struct {
	delays []time.Duration
}

func (t explicit) Backoff(attempt int) time.Duration {
	return t.delays[attempt%len(t.delays)]
}

// Attempt with a backoff strategy.
func Attempt(d Strategy, do func(int) int) {
	attempt := do(0)
	for {
		duration := d.Backoff(attempt)
		// log.Println("BACKOFF ATTEMPT SLEEPING", duration)
		time.Sleep(duration)
		attempt = do(attempt)
	}
}

// random backoff within the specified range.
func Random(d time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(d)))
}

// generate a *consistent* duration based on the input i within the
// provided window. this isn't the best location for these functions.
// but the lack of a better location.
func DynamicHashDuration(window time.Duration, i string) time.Duration {
	if window == 0 {
		return 0
	}

	return time.Duration(DynamicHashWindow(i, uint64(window)))
}

func DynamicHashHour(i string) time.Duration {
	return DynamicHashDuration(60*time.Minute, i)
}

func DynamicHash45m(i string) time.Duration {
	return DynamicHashDuration(45*time.Minute, i)
}

func DynamicHash15m(i string) time.Duration {
	return DynamicHashDuration(15*time.Minute, i)
}

func DynamicHash5m(i string) time.Duration {
	return DynamicHashDuration(5*time.Minute, i)
}

func DynamicHash1m(i string) time.Duration {
	return DynamicHashDuration(time.Minute, i)
}

func DynamicHashDay(i string) time.Weekday {
	return time.Weekday(DynamicHashWindow(i, 7))
}

// uint64 to prevent negative values
func DynamicHashWindow(i string, n uint64) uint64 {
	digest := md5.Sum([]byte(i))
	return binary.LittleEndian.Uint64(digest[:]) % n
}

func Iter(d Strategy) iter.Seq2[int, time.Duration] {
	return func(yield func(int, time.Duration) bool) {
		for i := 0; true; i++ {
			if !yield(i, d.Backoff(i)) {
				return
			}
		}
	}
}
