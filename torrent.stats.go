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

	// Ordered by expected descending quantities (if all is well).
	MaximumAllowedPeers int
	TotalPeers          int
	PendingPeers        int
	ActivePeers         int
	HalfOpenPeers       int
	Seeders             int

	Seeding        bool
	LastConnection time.Time
}

func (stats Stats) String() string {
	return fmt.Sprintf(
		"seeding(%t), peers(s%d:a%d:h%d:p%d:t%d) pieces(m%d:o%d:u%d:c%d - f%d)",
		stats.Seeding, stats.Seeders, stats.ActivePeers, stats.HalfOpenPeers, stats.PendingPeers, stats.TotalPeers,
		stats.Missing, stats.Outstanding, stats.Unverified, stats.Completed, stats.Failed,
	)
}
