package downloader

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stateSaveInterval is how often the saver goroutine snapshots the chunk
// done counters to the state file. It bounds crash-resume loss: after a
// hard kill the state is at most this stale, so at most this much wall
// time of downloaded bytes per active chunk is re-fetched on resume (and
// re-written idempotently — the saved counters always lag what is on
// disk, never lead it).
const stateSaveInterval = 500 * time.Millisecond

// ErrStateMismatch is returned (wrapped) when a state file exists but no
// longer matches the remote resource — the URL, size, or validators
// (ETag/Last-Modified) changed between sessions. Resuming would corrupt
// the output, so the caller must restart from scratch instead.
var ErrStateMismatch = errors.New("saved download state does not match the remote resource")

// state is the sidecar file persisted next to the output. done[i] is chunk
// i's contiguous completed-byte count — chunks fill front-to-back, so one
// number per chunk fully describes progress.
type state struct {
	URL          string  `json:"url"`
	Size         int64   `json:"size"`
	ETag         string  `json:"etag,omitempty"`
	LastModified string  `json:"lastModified,omitempty"`
	ChunkSize    int64   `json:"chunkSize"`
	Done         []int64 `json:"done"`
}

// StatePath returns the path of the sidecar state file for an output path.
func StatePath(output string) string {
	return output + ".mtd.json"
}

// loadState reads the state file at path. It returns (nil, nil) if the
// file does not exist; a corrupt file is an error rather than a silent
// restart so the user decides what to do with the partial download.
func loadState(path string) (*state, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("corrupt state file %s (delete it to restart): %w", path, err)
	}
	return &st, nil
}

// validate checks a loaded state against a fresh probe. Validators are
// compared only when both sides have them: a server that stopped sending
// an ETag falls back to the size check rather than failing.
func (st *state) validate(url string, probe ProbeResult) error {
	switch {
	case st.URL != url:
		return fmt.Errorf("%w: state is for %s", ErrStateMismatch, st.URL)
	case st.Size != probe.Size:
		return fmt.Errorf("%w: size changed from %d to %d", ErrStateMismatch, st.Size, probe.Size)
	case st.ETag != "" && probe.ETag != "" && st.ETag != probe.ETag:
		// ETags arrive already quoted, so %s keeps the message readable.
		return fmt.Errorf("%w: ETag changed from %s to %s", ErrStateMismatch, st.ETag, probe.ETag)
	case st.LastModified != "" && probe.LastModified != "" && st.LastModified != probe.LastModified:
		return fmt.Errorf("%w: Last-Modified changed from %q to %q", ErrStateMismatch, st.LastModified, probe.LastModified)
	case st.ChunkSize <= 0:
		return fmt.Errorf("%w: invalid chunk size %d", ErrStateMismatch, st.ChunkSize)
	}
	if want := len(buildChunks(st.Size, st.ChunkSize)); len(st.Done) != want {
		return fmt.Errorf("%w: %d chunk entries, expected %d", ErrStateMismatch, len(st.Done), want)
	}
	return nil
}

// restore applies the saved done counters to freshly built chunks, clamping
// defensively so a damaged counter can never mark bytes done beyond a
// chunk's length or resume from a negative offset.
func (st *state) restore(chunks []*chunk) {
	for i, c := range chunks {
		c.done.Store(min(max(st.Done[i], 0), c.length))
	}
}

// save atomically writes the state file: the snapshot goes to a temp file
// in the same directory, then rename replaces the old state in one step,
// so a crash mid-save can never leave a truncated state file behind.
func (st *state) save(path string) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// snapshot refreshes st.Done from the live chunk counters.
func (st *state) snapshot(chunks []*chunk) {
	for i, c := range chunks {
		st.Done[i] = c.done.Load()
	}
}

// runSaver persists the chunk counters every stateSaveInterval until stop
// is closed, then writes one final snapshot so a clean shutdown (Ctrl-C,
// error) loses nothing. Save errors are ignored mid-run — the next tick
// retries — because failing the download over a state write would cost
// more than the few re-downloaded bytes the stale state implies.
func (st *state) runSaver(path string, chunks []*chunk, stop <-chan struct{}) {
	ticker := time.NewTicker(stateSaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			st.snapshot(chunks)
			st.save(path)
		case <-stop:
			st.snapshot(chunks)
			st.save(path)
			return
		}
	}
}
