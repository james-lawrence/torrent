// Converts magnet URIs and info hashes into torrent metainfo files.
package main

import (
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/anacrolix/tagflag"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/bencode"
)

func main() {
	args := struct {
		tagflag.StartPos
		Magnet []string
	}{}
	tagflag.Parse(&args)
	cl, err := autobind.New().Bind(torrent.NewClient(nil))
	if err != nil {
		log.Fatalf("error creating client: %s", err)
	}
	http.HandleFunc("/torrent", func(w http.ResponseWriter, r *http.Request) {
		cl.WriteStatus(w)
	})
	http.HandleFunc("/dht", func(w http.ResponseWriter, r *http.Request) {
		for _, ds := range cl.DhtServers() {
			ds.WriteStatus(w)
		}
	})
	wg := sync.WaitGroup{}
	for _, arg := range args.Magnet {
		t, _, err := cl.MaybeStart(torrent.NewFromMagnet(arg))
		if err != nil {
			log.Fatalf("error adding magnet to client: %s", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-t.GotInfo()
			mi := t.Metainfo()
			cl.Stop(t.Metadata())
			f, err := os.Create(t.Info().Name + ".torrent")
			if err != nil {
				log.Fatalf("error creating torrent metainfo file: %s", err)
			}
			defer f.Close()
			err = bencode.NewEncoder(f).Encode(mi)
			if err != nil {
				log.Fatalf("error writing torrent metainfo file: %s", err)
			}
		}()
	}
	wg.Wait()
}
