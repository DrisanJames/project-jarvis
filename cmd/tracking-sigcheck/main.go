// Command tracking-sigcheck is the OFFLINE compatibility check for the
// /track/* signature verifier (internal/tracking/sigverify.go).
//
//	TRACKING_SECRET=<key[,oldkey]> tracking-sigcheck [-in urls.txt] [-key-env TRACKING_SECRET]
//
// Reads newline-delimited URLs (stdin by default) and prints ONLY counts per
// (host, kind, scheme, result), evaluating the enc and raw schemes
// independently. It never prints a token, subscriber id, destination or key.
// Read-only: no network, no DB.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/ignite/sparkpost-monitor/internal/tracking"
)

func main() {
	in := flag.String("in", "", "file of newline-delimited URLs (default stdin)")
	keyEnv := flag.String("key-env", tracking.SigKeyEnv, "env var holding the key (comma-separated for rotation)")
	flag.Parse()

	var r io.Reader = os.Stdin
	if *in != "" {
		f, err := os.Open(*in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open: %v\n", err)
			os.Exit(2)
		}
		defer f.Close()
		r = f
	}
	keys := tracking.ParseSigKeys(os.Getenv(*keyEnv))
	if err := run(r, os.Stdout, keys); err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
}

func run(r io.Reader, w io.Writer, keys [][]byte) error {
	tally, lines, err := tracking.TallyTrackingURLs(r, keys)
	if err != nil {
		return err
	}
	rows := make([]tracking.SigTallyKey, 0, len(tally))
	for k := range tally {
		rows = append(rows, k)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Scheme != b.Scheme {
			return a.Scheme < b.Scheme
		}
		return a.Result < b.Result
	})
	fmt.Fprintf(w, "lines=%d keys=%d\n", lines, len(keys))
	fmt.Fprintln(w, "host\tkind\tscheme\tresult\tcount")
	for _, k := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", k.Host, k.Kind, k.Scheme, k.Result, tally[k])
	}
	return nil
}
