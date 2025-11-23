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
	}
}

type memoryseeding struct {
	MetadataStore
	bm       BitmapStore
	_mu      *sync.RWMutex
	torrents map[int160.T]*torrent
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
		if err := t.bm.Write(id, c.chunks.ReadableBitmap()); err != nil {
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
	t._mu.RUnlock()

	if ok {
		return x, x.Tune(options...)
	}

	buildfn := func(id int160.T) (*torrent, error) {
		t._mu.Lock()
		defer t._mu.Unlock()

		if x, ok := t.torrents[id]; ok {
			return x, x.Tune(options...)
		}

		// only record if the info is there.
		if len(md.InfoBytes) > 0 {
			if err := t.MetadataStore.Write(md); err != nil {
				return nil, err
			}
		}

		x := fn(md, options...)
		t.torrents[id] = x

		return x, nil
	}

	dlt, err := buildfn(id)
	if err != nil {
		return nil, err
	}

	// if the bitmap cache exists read it to initialize
	unverified, err := t.bm.Read(id)
	if err != nil {
		return nil, err
	}

	return dlt, dlt.Tune(tuneVerifySample(unverified, 8))
}

func (t *memoryseeding) Load(id int160.T, fn func(md Metadata, options ...Tuner) *torrent, options ...Tuner) (dlt *torrent, cached bool, _ error) {
	t._mu.RLock()
	x, ok := t.torrents[id]
	t._mu.RUnlock()

	if ok {
		return x, true, x.Tune(options...)
	}

	buildfn := func(id int160.T) (*torrent, bool, error) {
		t._mu.Lock()
		defer t._mu.Unlock()

		if x, ok := t.torrents[id]; ok {
			return x, true, x.Tune(options...)
		}

		md, err := t.MetadataStore.Read(id)
		if err != nil {
			return nil, false, err
		}

		x := fn(md, options...)
		t.torrents[id] = x

		return x, false, nil
	}

	var (
		err error
	)

	if dlt, cached, err = buildfn(id); err != nil {
		return dlt, false, err
	}

	unverified, err := t.bm.Read(id)
	if err != nil {
		return nil, false, err
	}

	return dlt, cached, dlt.Tune(tuneVerifySample(unverified, 8))
}

func (t *memoryseeding) Metadata(id int160.T) (md Metadata, err error) {
	t._mu.RLock()
	defer t._mu.RUnlock()

	if x, ok := t.torrents[id]; ok {
		return x.md, nil
	}

	return t.MetadataStore.Read(id)
}
