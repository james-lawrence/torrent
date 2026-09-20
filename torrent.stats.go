package torrent

import (
	"fmt"
	"time"
)

// Stats high level stats about the torrent.
type Stats struct {
	// Aggregates stats over all connections past and present. Some values may
	// not have much meaning in the aggregate context.
	ConnStats

	// metrics marking the progress of the torrent
	// these are in chunks.
	Missing     int
	Outstanding int
	Unverified  int
	Failed      int
	Completed   int

	// Downloaded number of bytes of the entire torrent we have completed. This is the sum of
	// the pieces that are known to be good: completed pieces, and pieces credited when the
	// torrent was resumed. Chunks of incomplete pieces and unverified chunks are not included.
	// Do not use this for download rate, as it can go down when pieces are lost or fail checks.
	// Sample ConnStats.DataBytesRead for actual file data download rate.
	Downloaded int64

	// DownloadedOptimistic number of bytes of the entire torrent we have, assuming the unverified
	// chunks are good. This is the sum of completed pieces and unverified chunks, and can
	// overcount when an unverified chunk turns out to be bad or the last chunk is short.
	DownloadedOptimistic int64

	// Remaining number of bytes of the entire torrent still to be downloaded.
	Remaining int64

	// Ordered by expected descending quantities (if all is well).
	MaximumAllowedPeers int
	TotalPeers          int
	PendingPeers        int
	ActivePeers         int
	HalfOpenPeers       int
	Seeders             int

	Seeding        bool
	LastConnection time.Time
	// last connection change or validated piece, used for idle unloading.
	LastActivity time.Time
}

func (stats Stats) String() string {
	return fmt.Sprintf(
		"seeding(%t), peers(s%d:a%d:h%d:p%d:t%d) pieces(m%d:o%d:u%d:c%d - f%d)",
		stats.Seeding, stats.Seeders, stats.ActivePeers, stats.HalfOpenPeers, stats.PendingPeers, stats.TotalPeers,
		stats.Missing, stats.Outstanding, stats.Unverified, stats.Completed, stats.Failed,
	)
}
