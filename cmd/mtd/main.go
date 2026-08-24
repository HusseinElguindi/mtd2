// Command mtd is a concurrent, resumable HTTP downloader: it fetches a
// URL in parallel byte-range chunks and persists progress next to the
// output file, so an interrupted download resumes by rerunning the same
// command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"time"

	"mtd2/downloader"
)

// dumpTransport prints each request and response (headers only, as they
// go over the wire) to stderr, for debugging servers that reject the
// probe. Redirect hops are visible too: the transport sits below the
// client's redirect handling, so every hop passes through here.
type dumpTransport struct {
	rt http.RoundTripper
}

func (d dumpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if out, err := httputil.DumpRequestOut(req, false); err == nil {
		fmt.Fprintf(os.Stderr, "> %s\n", strings.ReplaceAll(strings.TrimSpace(string(out)), "\r\n", "\n> "))
	}
	resp, err := d.rt.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "< error: %v\n", err)
		return resp, err
	}
	if out, derr := httputil.DumpResponse(resp, false); derr == nil {
		fmt.Fprintf(os.Stderr, "< %s\n", strings.ReplaceAll(strings.TrimSpace(string(out)), "\r\n", "\n< "))
	}
	return resp, err
}

func main() {
	output := flag.String("o", "", "output file (default: last URL path element)")
	concurrency := flag.Int("c", 8, "number of parallel connections")
	chunkSize := flag.String("s", "", "chunk size, e.g. 16MiB (default: derived from file size)")
	restart := flag.Bool("restart", false, "discard any saved state and partial file, start over")
	force := flag.Bool("f", false, "overwrite an existing output file")
	verbose := flag.Bool("v", false, "dump HTTP request/response headers to stderr")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: mtd [-o output] [-c concurrency] [-s chunk-size] [-f] [--restart] <url>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *output, *concurrency, *chunkSize, *restart, *force, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "mtd:", err)
		os.Exit(1)
	}
}

func run(rawURL, output string, concurrency int, chunkSizeArg string, restart, force, verbose bool) error {
	if output == "" {
		var err error
		if output, err = defaultOutput(rawURL); err != nil {
			return err
		}
	}
	var chunkSize int64
	if chunkSizeArg != "" {
		var err error
		if chunkSize, err = parseSize(chunkSizeArg); err != nil {
			return fmt.Errorf("-s: %w", err)
		}
	}
	if restart {
		// Restart discards this download's own artifacts; a file at the
		// final name is someone's completed data and still requires -f.
		os.Remove(downloader.PartPath(output))
		os.Remove(downloader.StatePath(output))
	}

	var client *http.Client
	if verbose {
		// The library's default client with the dumping transport layered
		// underneath, so -v changes only visibility, not behavior. The
		// dump wraps the downloader's own tuned transport, not
		// http.DefaultTransport: wrapping the stock one would quietly
		// hand -v runs an undersized connection pool.
		var err error
		if client, err = downloader.NewClient(dumpTransport{downloader.NewTransport(concurrency)}); err != nil {
			return err
		}
	}
	d, err := downloader.New(downloader.Options{
		URL:         rawURL,
		Output:      output,
		Concurrency: concurrency,
		ChunkSize:   chunkSize,
		Client:      client,
		Force:       force,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	r := newRenderer(os.Stderr)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	var runErr error
loop:
	for {
		select {
		case runErr = <-done:
			break loop
		case <-ticker.C:
			r.render(d.Progress())
		}
	}
	r.render(d.Progress())
	r.finish()

	if runErr != nil {
		if errors.Is(runErr, downloader.ErrOutputExists) {
			return fmt.Errorf("%w — use -o for a different name, -f to overwrite, or --restart to discard a partial download", runErr)
		}
		if errors.Is(runErr, context.Canceled) {
			if _, err := os.Stat(downloader.StatePath(output)); err == nil {
				return fmt.Errorf("interrupted — progress saved; rerun the same command to resume")
			}
			return errors.New("interrupted")
		}
		return runErr
	}
	p := d.Progress()
	fmt.Fprintf(os.Stderr, "downloaded %s to %s in %s (%s/s)\n",
		fmtBytes(p.Downloaded), output, time.Since(start).Round(time.Second), fmtBytes(int64(p.AvgRate)))
	return nil
}

// defaultOutput derives an output filename from the URL's last path
// element.
func defaultOutput(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if name := path.Base(u.Path); name != "" && name != "/" && name != "." {
		return name, nil
	}
	return "", errors.New("cannot derive an output name from the URL; use -o")
}

// parseSize parses a human-readable byte size: plain bytes, or a decimal
// (KB/MB/GB) or binary (KiB/MiB/GiB) suffix.
func parseSize(s string) (int64, error) {
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
	}
	for _, u := range suffixes {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil || f <= 0 {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n, nil
}
