// Package backup writes and reads gzipped Cypher dumps of the graph to a host
// directory outside the Docker volume, so losing the volume does not lose the
// memories. It never touches Bolt: the caller supplies the statements.
package backup

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	filePrefix = "imem-"
	fileSuffix = ".cypherl.gz"
	// stampFormat is UTC so filenames sort lexically in chronological order
	// and never repeat or reorder across a DST change.
	stampFormat = "20060102-150405"
	glob        = filePrefix + "*" + fileSuffix
	// maxLine bounds the scanner: a single Cypher statement carrying a long
	// memory body can far exceed bufio's 64KB default.
	maxLine = 8 << 20
)

// nodePrefix marks a node-creating statement in a Memgraph dump.
const nodePrefix = "CREATE (:__mg_vertex__"

// EscapeControl rewrites raw control bytes as Cypher \uXXXX escapes.
//
// This is not cosmetic. Entity.key joins the project key and the name with a
// NUL separator, and DUMP DATABASE emits that byte raw inside the string
// literal — which Memgraph's own parser then rejects ("wrong token"), so an
// unescaped dump cannot be replayed, by imem or by mgconsole. Memgraph does
// accept \u0000 and decodes it back to the same byte, so escaping on write
// makes the stored file both valid Cypher and byte-exact on restore.
//
// Escaping every control byte (not just NUL) is safe because DUMP DATABASE
// emits one statement per line with space-separated tokens: a control byte
// can only ever appear inside a string literal. It also guarantees each
// statement stays on a single line in the .cypherl file.
func EscapeControl(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			fmt.Fprintf(&b, `\u%04X`, c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// HasData reports whether a dump carries actual graph data. A dump of a wiped
// Memgraph is not empty: the daemon's EnsureSchema recreates indexes and
// constraints at boot, so a blank database still dumps ~12 DDL statements.
// Callers use this instead of a length check before overwriting good backups.
func HasData(stmts []string) bool {
	for _, s := range stmts {
		if strings.HasPrefix(s, nodePrefix) {
			return true
		}
	}
	return false
}

type Info struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Write serialises stmts to <dir>/imem-<stamp>.cypherl.gz. It builds a dotfile
// temp first and renames, so a crash mid-write can never leave a truncated
// file that looks like a usable backup.
func Write(dir string, stmts []string, now time.Time) (Info, error) {
	if len(stmts) == 0 {
		return Info{}, fmt.Errorf("refusing to write an empty backup")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Info{}, err
	}
	name := filePrefix + now.UTC().Format(stampFormat) + fileSuffix
	final := filepath.Join(dir, name)
	tmp := filepath.Join(dir, "."+name+".tmp")

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return Info{}, err
	}
	err = func() error {
		zw, err := gzip.NewWriterLevel(f, gzip.BestCompression)
		if err != nil {
			return err
		}
		bw := bufio.NewWriter(zw)
		for _, s := range stmts {
			if _, err := bw.WriteString(EscapeControl(s)); err != nil {
				return err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return err
			}
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		return f.Sync()
	}()
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return Info{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return Info{}, err
	}
	fi, err := os.Stat(final)
	if err != nil {
		return Info{}, err
	}
	return Info{Path: final, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Load reads a backup back into its statements. Lines are reassembled until
// the buffer ends with ';' so the reader stays correct even if a raw newline
// ever appears inside a string literal.
func Load(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	var stmts []string
	var buf strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(line)
		if strings.HasSuffix(strings.TrimRight(buf.String(), " \t"), ";") {
			if s := strings.TrimSpace(buf.String()); s != "" {
				stmts = append(stmts, s)
			}
			buf.Reset()
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts, nil
}

// List returns the backups in dir, newest first. Filenames carry a UTC stamp,
// so a descending name sort is a chronological sort and does not depend on
// mtimes surviving a copy.
func List(dir string) ([]Info, error) {
	matches, err := filepath.Glob(filepath.Join(dir, glob))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	out := make([]Info, 0, len(matches))
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		out = append(out, Info{Path: p, Size: fi.Size(), ModTime: fi.ModTime()})
	}
	return out, nil
}

// Latest returns the newest backup, if any.
func Latest(dir string) (Info, bool, error) {
	list, err := List(dir)
	if err != nil || len(list) == 0 {
		return Info{}, false, err
	}
	return list[0], true, nil
}

// Prune deletes everything past the keep newest. Callers write first and prune
// second, so the new backup is already durable before an old one is dropped.
func Prune(dir string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	list, err := List(dir)
	if err != nil {
		return err
	}
	for _, in := range list[min(keep, len(list)):] {
		if err := os.Remove(in.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// CleanTemp removes dotfile temps abandoned by a crash mid-write.
func CleanTemp(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "."+filePrefix+"*"+fileSuffix+".tmp"))
	if err != nil {
		return err
	}
	for _, p := range matches {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
