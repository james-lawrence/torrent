package torrent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/torrenttest"
	"github.com/stretchr/testify/require"
)

func TestTorrentCache(t *testing.T) {
	t.Run("create and close empty cache", func(t *testing.T) {
		tmpdir := t.TempDir()

		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		require.NoError(t, c.Close())
	})

	t.Run("insert new torrent", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)
		require.EqualValues(t, 0, len(c.torrents))
		tor, err := c.Insert(md, zeroTorrent)
		require.NoError(t, err)
		require.NotNil(t, tor)
		require.EqualValues(t, 1, len(c.torrents))
		require.NoError(t, c.Close())
	})

	t.Run("insert existing torrent returns cached one", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)

		tor1, err := c.Insert(md, zeroTorrent)
		require.NoError(t, err)

		tor2, err := c.Insert(md, zeroTorrent)
		require.NoError(t, err)
		require.True(t, tor1 == tor2)
		require.EqualValues(t, 1, len(c.torrents))
		require.NoError(t, c.Close())
	})

	t.Run("load existing torrent from memory", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)
		tor1, err := c.Insert(md, zeroTorrent)
		require.NoError(t, err)

		tor2, cached, err := c.Load(md.ID, zeroTorrent)
		require.NoError(t, err)
		require.True(t, cached)
		require.True(t, tor1 == tor2)
		require.NoError(t, c.Close())
	})

	t.Run("load torrent from store into memory", func(t *testing.T) {
		tmpdir := t.TempDir()
		mdStore := NewMetadataCache(tmpdir)
		c := NewCache(mdStore, NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)
		require.NoError(t, mdStore.Write(md))

		require.EqualValues(t, 0, len(c.torrents))
		tor, cached, err := c.Load(md.ID, zeroTorrent)
		require.NoError(t, err)
		require.False(t, cached)
		require.NotNil(t, tor)
		require.EqualValues(t, 1, len(c.torrents))
		require.NoError(t, c.Close())
	})

	t.Run("drop non-existent torrent", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))
		id := int160.FromBytes([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
		require.NoError(t, c.Drop(id))
		require.NoError(t, c.Close())
	})

	t.Run("drop existent torrent", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)
		_, err = c.Insert(md, zeroTorrent)
		require.NoError(t, err)
		require.EqualValues(t, 1, len(c.torrents))
		require.NoError(t, c.Drop(md.ID))
		require.EqualValues(t, 0, len(c.torrents))
		require.NoError(t, c.Close())
	})

	t.Run("sync non-existent torrent", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))
		id := int160.FromBytes([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
		require.NoError(t, c.Sync(id))
		require.NoError(t, c.Close())
	})

	t.Run("metadata for torrent in memory", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md1, err := NewFromInfo(info)
		require.NoError(t, err)
		_, err = c.Insert(md1, zeroTorrent)
		require.NoError(t, err)

		md2, err := c.Metadata(md1.ID)
		require.NoError(t, err)
		require.Equal(t, md1.ID, md2.ID)
		require.NoError(t, c.Close())
	})

	t.Run("metadata for torrent not in memory", func(t *testing.T) {
		tmpdir := t.TempDir()
		mdStore := NewMetadataCache(tmpdir)
		c := NewCache(mdStore, NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md1, err := NewFromInfo(info)
		require.NoError(t, err)
		require.NoError(t, mdStore.Write(md1))

		md2, err := c.Metadata(md1.ID)
		require.NoError(t, err)
		require.Equal(t, md1.ID, md2.ID)
		require.NoError(t, c.Close())
	})

	t.Run("concurrent inserts", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(10)
		torrents := make(chan *torrent, 10)
		start := make(chan struct{})
		blocked := make(chan struct{})
		for range 10 {
			go func() {
				<-start
				tor, err := c.Insert(md, func(md Metadata, options ...Tuner) *torrent {
					<-blocked
					return zeroTorrent(md, options...)
				})
				require.NoError(t, err)
				torrents <- tor
				wg.Done()
			}()
		}
		// wake all the insert routines.
		close(start)
		// close the blocking channel allowing the insert to complete
		close(blocked)

		wg.Wait()
		close(torrents)

		var firstTor *torrent
		for tor := range torrents {
			if firstTor == nil {
				firstTor = tor
			}
			require.True(t, firstTor == tor)
		}

		require.EqualValues(t, 1, len(c.torrents))
		require.NoError(t, c.Close())
	})

	t.Run("concurrent load and insert", func(t *testing.T) {
		tmpdir := t.TempDir()
		mdc := NewMetadataCache(tmpdir)
		bmc := NewBitmapCache(tmpdir)
		c := NewCache(mdc, bmc)

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)
		require.NoError(t, mdc.Write(md))

		var wg sync.WaitGroup
		wg.Add(2)
		torrents := make(chan *torrent, 2)

		start := make(chan struct{})
		blocked := make(chan struct{})

		go func() {
			<-start
			tor, err := c.Insert(md, func(md Metadata, options ...Tuner) *torrent {
				<-blocked
				return zeroTorrent(md, options...)
			})
			require.NoError(t, err)
			torrents <- tor
			wg.Done()
		}()

		go func() {
			<-start
			tor, _, err := c.Load(md.ID, func(md Metadata, options ...Tuner) *torrent {
				<-blocked
				return zeroTorrent(md, options...)
			})
			require.NoError(t, err)
			torrents <- tor
			wg.Done()
		}()

		// wake all the insert routines.
		close(start)
		// close the blocking channel allowing the insert to complete
		close(blocked)

		wg.Wait()
		close(torrents)

		var firstTor *torrent
		for tor := range torrents {
			if firstTor == nil {
				firstTor = tor
			}
			require.True(t, firstTor == tor)
		}

		require.EqualValues(t, 1, len(c.torrents))
		require.NoError(t, c.Close())
	})

	t.Run("concurrent metadata reads", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)
		_, err = c.Insert(md, zeroTorrent)
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(10)

		for range 10 {
			go func() {
				mdRead, err := c.Metadata(md.ID)
				require.NoError(t, err)
				require.Equal(t, md.ID, mdRead.ID)
				wg.Done()
			}()
		}

		wg.Wait()
		require.NoError(t, c.Close())
	})

	t.Run("concurrent close", func(t *testing.T) {
		tmpdir := t.TempDir()

		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info1, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md1, err := NewFromInfo(info1)
		require.NoError(t, err)
		_, err = c.Insert(md1, zeroTorrent)
		require.NoError(t, err)

		info2, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md2, err := NewFromInfo(info2)
		require.NoError(t, err)
		_, err = c.Insert(md2, zeroTorrent)
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(2)
		var err1, err2 error

		go func() {
			err1 = c.Close()
			wg.Done()
		}()

		go func() {
			err2 = c.Close()
			wg.Done()
		}()

		wg.Wait()

		require.True(t, err1 == nil || err2 == nil)
	})

	t.Run("concurrent sync and insert", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			for {
				select {
				case <-ctx.Done():
					wg.Done()
					return
				default:
					c.Sync(md.ID)
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()

		go func() {
			for {
				select {
				case <-ctx.Done():
					wg.Done()
					return
				default:
					c.Insert(md, zeroTorrent)
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()

		time.Sleep(100 * time.Millisecond)
		cancel()
		wg.Wait()
		require.NoError(t, c.Close())
	})

	t.Run("concurrent drop and insert", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			c.Insert(md, zeroTorrent)
			wg.Done()
		}()

		go func() {
			c.Drop(md.ID)
			wg.Done()
		}()

		wg.Wait()
		require.NoError(t, c.Close())
	})

	t.Run("concurrent drop and sync", func(t *testing.T) {
		tmpdir := t.TempDir()
		c := NewCache(NewMetadataCache(tmpdir), NewBitmapCache(tmpdir))

		info, _, err := torrenttest.Random(tmpdir, 1)
		require.NoError(t, err)
		md, err := NewFromInfo(info)
		require.NoError(t, err)
		_, err = c.Insert(md, zeroTorrent)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			for {
				select {
				case <-ctx.Done():
					wg.Done()
					return
				default:
					c.Drop(md.ID)
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()

		go func() {
			for {
				select {
				case <-ctx.Done():
					wg.Done()
					return
				default:
					c.Sync(md.ID)
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()

		time.Sleep(100 * time.Millisecond)
		cancel()
		wg.Wait()
		require.NoError(t, c.Close())
	})
}
