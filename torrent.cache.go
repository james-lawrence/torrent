package torrent

import (
	"sync"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/langx"
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
	t._mu.RLock()
	c, ok := t.torrents[id]
	t._mu.RUnlock()

	if !ok {
		return nil
	}

	t._mu.Lock()
	delete(t.torrents, id)
	t._mu.Unlock()

	// only record if the info is there.
	if c.haveInfo() {
		if err := t.bm.Write(id, c.chunks.ReadableBitmap()); err != nil {
			return err
		}
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

	t._mu.Lock()
	defer t._mu.Unlock()
	// only record if the info is there.
	if len(md.InfoBytes) > 0 {
		if err := t.MetadataStore.Write(md); err != nil {
			return nil, err
		}
	}

	// if the bitmap cache exists read it to initialize
	unverified, err := t.bm.Read(id)
	if err != nil {
		return nil, err
	}

	dlt := fn(md, tuneVerifySample(unverified, 8), langx.Compose(options...))
	t.torrents[id] = dlt

	return dlt, nil
}

func (t *memoryseeding) Load(id int160.T, fn func(md Metadata, options ...Tuner) *torrent, options ...Tuner) (_ *torrent, cached bool, _ error) {
	t._mu.RLock()
	x, ok := t.torrents[id]
	t._mu.RUnlock()

	if ok {
		return x, true, x.Tune(options...)
	}

	md, err := t.MetadataStore.Read(id)
	if err != nil {
		return nil, false, err
	}

	unverified, err := t.bm.Read(id)
	if err != nil {
		return nil, false, err
	}

	t._mu.Lock()
	defer t._mu.Unlock()

	if x, ok := t.torrents[id]; ok {
		return x, true, x.Tune(options...)
	}

	dlt := fn(md, tuneVerifySample(unverified, 8), langx.Compose(options...))
	t.torrents[id] = dlt

	return dlt, false, nil
}

func (t *memoryseeding) Metadata(id int160.T) (md Metadata, err error) {
	t._mu.Lock()
	defer t._mu.Unlock()

	if x, ok := t.torrents[id]; ok {
		return x.md, nil
	}

	return t.MetadataStore.Read(id)
}
