package torrent

import (
	"sync"

	"github.com/james-lawrence/torrent/dht/int160"
)

func NewCache(s MetadataStore, b BitmapStore) *memoryseeding {
	return &memoryseeding{
		_mu:           &sync.RWMutex{},
		MetadataStore: s,
		bm:            b,
		torrents:      make(map[int160.T]*torrent, 128),
		initializing:  make(map[int160.T]chan struct{}, 128),
	}
}

type memoryseeding struct {
	MetadataStore
	bm       BitmapStore
	_mu      *sync.RWMutex
	torrents map[int160.T]*torrent
	// torrents that are in the map but still loading their initial state (bitmap, sample verify).
	// the channel is closed once the torrent is ready. anyone else asking for the torrent waits on
	// it, otherwise they are handed a torrent that claims to have no data and announce that to
	// every peer that connects while it loads.
	initializing map[int160.T]chan struct{}
}

// ready marks the torrent as finished initializing, releasing everyone waiting on it.
func (t *memoryseeding) ready(id int160.T, gate chan struct{}) {
	t._mu.Lock()
	// a drop followed by a reload may have replaced the entry with a newer torrent's gate.
	if t.initializing[id] == gate {
		delete(t.initializing, id)
	}
	t._mu.Unlock()

	close(gate)
}

func (t *memoryseeding) Close() error {
	t._mu.Lock()
	defer t._mu.Unlock()

	for _, c := range t.torrents {
		if err := c.close(); err != nil {
			return err
		}
	}

	return nil
}

// sync bitmap to disk
func (t *memoryseeding) Sync(id int160.T) error {
	t._mu.Lock()
	defer t._mu.Unlock()
	c, ok := t.torrents[id]

	if !ok {
		return nil
	}

	if c.haveInfo() {
		if err := t.bm.Write(id, c.chunks.Read(DownloadedSnapshotSave)); err != nil {
			return err
		}
	}

	return nil
}

// clear torrent from memory
func (t *memoryseeding) Drop(id int160.T) error {
	if err := t.Sync(id); err != nil {
		return err
	}

	t._mu.Lock()
	c, ok := t.torrents[id]
	delete(t.torrents, id)
	t._mu.Unlock()
	if !ok {
		return nil
	}

	return c.close()
}

func (t *memoryseeding) Insert(md Metadata, fn func(md Metadata, options ...Tuner) *torrent, options ...Tuner) (*torrent, error) {
	id := int160.FromBytes(md.ID.Bytes())
	t._mu.RLock()
	x, ok := t.torrents[id]
	gate := t.initializing[id]
	t._mu.RUnlock()

	if ok {
		if gate != nil {
			<-gate
		}

		return x, x.Tune(options...)
	}

	// returns the torrent, and the gate for it when the torrent was already being created by someone else.
	buildfn := func(id int160.T) (*torrent, chan struct{}, bool, error) {
		t._mu.Lock()
		defer t._mu.Unlock()

		if x, ok := t.torrents[id]; ok {
			return x, t.initializing[id], true, nil
		}

		// only record if the info is there.
		if len(md.InfoBytes) > 0 {
			if err := t.MetadataStore.Write(md); err != nil {
				return nil, nil, false, err
			}
		}

		x := fn(md, options...)
		gate := make(chan struct{})
		t.torrents[id] = x
		t.initializing[id] = gate

		return x, gate, false, nil
	}

	dlt, gate, existing, err := buildfn(id)
	if err != nil {
		return nil, err
	}

	// someone else created the torrent, they are responsible for initializing it.
	if existing {
		if gate != nil {
			<-gate
		}

		return dlt, dlt.Tune(options...)
	}

	defer t.ready(id, gate)

	// if the bitmap cache exists read it to initialize
	unverified, err := t.bm.Read(id)
	if err != nil {
		return nil, err
	}

	return dlt, dlt.Tune(TuneVerifyBitmap(unverified, 8))
}

func (t *memoryseeding) Load(id int160.T, fn func(md Metadata, options ...Tuner) *torrent, options ...Tuner) (dlt *torrent, cached bool, _ error) {
	t._mu.RLock()
	x, ok := t.torrents[id]
	gate := t.initializing[id]
	t._mu.RUnlock()

	if ok {
		if gate != nil {
			<-gate
		}

		return x, true, x.Tune(options...)
	}

	// returns the torrent, and the gate for it when the torrent was already being created by someone else.
	buildfn := func(id int160.T) (*torrent, chan struct{}, bool, error) {
		t._mu.Lock()
		defer t._mu.Unlock()

		if x, ok := t.torrents[id]; ok {
			return x, t.initializing[id], true, nil
		}

		md, err := t.MetadataStore.Read(id)
		if err != nil {
			return nil, nil, false, err
		}

		x := fn(md, options...)
		gate := make(chan struct{})
		t.torrents[id] = x
		t.initializing[id] = gate

		return x, gate, false, nil
	}

	var (
		err error
	)

	if dlt, gate, cached, err = buildfn(id); err != nil {
		return dlt, false, err
	}

	// someone else created the torrent, they are responsible for initializing it.
	if cached {
		if gate != nil {
			<-gate
		}

		return dlt, true, dlt.Tune(options...)
	}

	defer t.ready(id, gate)

	unverified, err := t.bm.Read(id)
	if err != nil {
		return nil, false, err
	}

	return dlt, cached, dlt.Tune(TuneVerifyBitmap(unverified, 8))
}

func (t *memoryseeding) Metadata(id int160.T) (md Metadata, err error) {
	t._mu.RLock()
	defer t._mu.RUnlock()

	if x, ok := t.torrents[id]; ok {
		return x.md, nil
	}

	return t.MetadataStore.Read(id)
}
