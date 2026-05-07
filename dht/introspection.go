package dht

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/internal/langx"
)

func prettySince(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	d /= time.Second
	d *= time.Second
	return fmt.Sprintf("%s ago", d)
}

func Dump(s *Server, dst io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := fmt.Fprintf(dst, "DHT Server (node id %s) (bindings %d)\n", int160.StableSuffix(langx.Zero(s.id.Load())), len(s.bindings)); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(dst, "Ongoing transactions: %d\n", s.transactions.NumActive()); err != nil {
		return err
	}

	for _, b := range s.bindings {
		if err := dumpBinding(b, dst); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintln(dst); err != nil {
		return err
	}

	return nil
}

func dumpBinding(b *socketbinding, dst io.Writer) error {
	root := b.ID()
	id := string([]rune(int160.SecurePrefix(root).String())[:6])
	if _, err := fmt.Fprintf(dst, "binding %s - listen(%s) public(%s) nodes(%d)\n", id, b.pc.LocalAddr(), b.AddrPort(), b.Routing().numNodes()); err != nil {
		return err
	}

	for i := range b.table.buckets {
		if err := dumpBucket(root, &b.table.buckets[i], i, b.table, dst); err != nil {
			return err
		}
	}
	return nil
}

func dumpBucket(root int160.T, b *bucket, index int, tbl *table, dst io.Writer) error {
	if b.Len() == 0 && b.lastChanged.IsZero() {
		return nil
	}

	id := string([]rune(int160.SecurePrefix(root).String())[:5])
	if _, err := fmt.Fprintf(dst, "b%s#%v: %v nodes, last updated: %v\n", id, index, b.Len(), prettySince(b.lastChanged)); err != nil {
		return err
	}
	if b.Len() == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(dst, 1, 0, 1, ' ', 0)
	fmt.Fprintf(tw, "  node id\taddr\tlast query\tlast response\trecv\tdiscard\tflags\n")
	nodes := slices.SortedFunc(b.NodeIter(), func(l *node, r *node) int {
		return l.Id.Distance(root).Cmp(r.Id.Distance(root))
	})
	for _, n := range nodes {
		var flags []string
		if tbl.isQuestionable(root, n) {
			flags = append(flags, "q")
		}
		if nodeIsBad(root, n) {
			flags = append(flags, "b")
		}
		if tbl.isGood(root, n) {
			flags = append(flags, "g")
		}
		if n.IsSecure() {
			flags = append(flags, "sec")
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%v\t%v\n",
			n.Id,
			n.Addr,
			prettySince(n.lastGotQuery),
			prettySince(n.lastGotResponse),
			n.numReceivesFrom,
			n.failedLastQuestionablePing,
			strings.Join(flags, ","),
		)
	}
	return tw.Flush()
}
