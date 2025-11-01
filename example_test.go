package torrent_test

import (
	"context"
	"io"
	"log"
	"net"
	"os"

	"github.com/james-lawrence/torrent"
	"github.com/james-lawrence/torrent/autobind"
	"github.com/james-lawrence/torrent/sockets"
	"github.com/james-lawrence/torrent/storage"
)

func Example_download() {
	var (
		err      error
		metadata torrent.Metadata
	)

	c, _ := autobind.NewDefaultClient()
	defer c.Close()

	if metadata, err = torrent.NewFromMagnet("magnet:?xt=urn:btih:ZOCMZQIPFFW7OLLMIC5HUB6BPCSDEOQU"); err != nil {
		log.Fatalln(err)
		return
	}
	dl, _, err := c.Start(metadata)
	if err != nil {
		log.Fatalln(err)
		return
	}

	if _, err = torrent.DownloadInto(context.Background(), io.Discard, dl); err != nil {
		// if _, err = c.DownloadInto(context.Background(), metadata, io.Discard); err != nil {
		log.Fatalln(err)
		return
	}

	log.Print("torrent downloaded")
}

func Example_customNetworkProtocols() {
	var (
		err      error
		metadata torrent.Metadata
	)

	l, err := net.Listen("unix", "")
	if err != nil {
		log.Fatalln(err)
		return
	}
	defer l.Close()

	s := sockets.New(l, &net.Dialer{LocalAddr: l.Addr()})
	c, _ := torrent.NewSocketsBind(s).Bind(torrent.NewClient(torrent.NewDefaultClientConfig(
		torrent.NewMetadataCache(os.TempDir()),
		storage.NewFile(os.TempDir()),
		torrent.ClientConfigBootstrapGlobal,
	)))
	defer c.Close()

	if metadata, err = torrent.NewFromMagnet("magnet:?xt=urn:btih:ZOCMZQIPFFW7OLLMIC5HUB6BPCSDEOQU"); err != nil {
		log.Fatalln(err)
		return
	}

	dl, _, err := c.Start(metadata)
	if err != nil {
		log.Fatalln(err)
		return
	}

	if _, err = torrent.DownloadInto(context.Background(), io.Discard, dl); err != nil {
		log.Fatalln(err)
		return
	}

	log.Print("torrent downloaded")
}

func Example_fileReader() {
	var f torrent.File
	// Accesses the parts of the torrent pertaining to f. Data will be
	// downloaded as required, per the configuration of the torrent.Reader.
	r := f.NewReader()
	defer r.Close()
}
