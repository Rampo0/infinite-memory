package backup

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stamp(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(stampFormat, s)
	if err != nil {
		t.Fatalf("bad stamp %q: %v", s, err)
	}
	return tm
}

func TestWriteLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stmts := []string{
		"CREATE (:__mg_vertex__:`Memory` {__mg_id__: 1, `content`: \"em dash — ellipsis …\"});",
		`MATCH (u) REMOVE u:__mg_vertex__, u.__mg_id__;`,
		"CREATE CONSTRAINT ON (u:`Memory`) ASSERT u.`hash` IS UNIQUE;",
	}
	info, err := Write(dir, stmts, stamp(t, "20260824-140000"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(info.Path) != "imem-20260824-140000.cypherl.gz" {
		t.Fatalf("unexpected name %s", info.Path)
	}
	if info.Size == 0 {
		t.Fatal("zero size")
	}
	got, err := Load(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(stmts) {
		t.Fatalf("got %d statements, want %d: %q", len(got), len(stmts), got)
	}
	for i := range stmts {
		if got[i] != stmts[i] {
			t.Errorf("stmt %d:\n got %q\nwant %q", i, got[i], stmts[i])
		}
	}
}

func TestWriteRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, nil, time.Now()); err == nil {
		t.Fatal("want error for empty dump")
	}
	if n := len(mustList(t, dir)); n != 0 {
		t.Fatalf("wrote %d files for an empty dump", n)
	}
}

func TestWriteIsAtomicNoTempLeft(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, []string{"RETURN 1;"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// Load must survive a raw newline inside a literal by joining lines until the
// buffer terminates with a semicolon.
func TestLoadReassemblesSplitStatement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imem-20260824-000000.cypherl.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte("CREATE (:A {t: \"one\ntwo\"});\nRETURN 1;\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CREATE (:A {t: \"one\ntwo\"});", "RETURN 1;"}
	if len(got) != len(want) {
		t.Fatalf("got %d statements %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stmt %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestListNewestFirstIgnoresStrays(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{"20260824-100000", "20260824-140000", "20260823-090000"} {
		if _, err := Write(dir, []string{"RETURN 1;"}, stamp(t, s)); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, ".imem-20260825-000000.cypherl.gz.tmp"), "junk")
	write(t, filepath.Join(dir, "notes.txt"), "junk")

	list := mustList(t, dir)
	want := []string{
		"imem-20260824-140000.cypherl.gz",
		"imem-20260824-100000.cypherl.gz",
		"imem-20260823-090000.cypherl.gz",
	}
	if len(list) != len(want) {
		t.Fatalf("got %d entries, want %d", len(list), len(want))
	}
	for i, w := range want {
		if got := filepath.Base(list[i].Path); got != w {
			t.Errorf("position %d: got %s want %s", i, got, w)
		}
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	stamps := []string{"20260820-000000", "20260821-000000", "20260822-000000", "20260823-000000", "20260824-000000"}
	for _, s := range stamps {
		if _, err := Write(dir, []string{"RETURN 1;"}, stamp(t, s)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Prune(dir, 2); err != nil {
		t.Fatal(err)
	}
	list := mustList(t, dir)
	if len(list) != 2 {
		t.Fatalf("got %d backups after prune, want 2", len(list))
	}
	want := []string{"imem-20260824-000000.cypherl.gz", "imem-20260823-000000.cypherl.gz"}
	for i, w := range want {
		if got := filepath.Base(list[i].Path); got != w {
			t.Errorf("position %d: got %s want %s", i, got, w)
		}
	}
}

func TestPruneEmptyDirIsNoop(t *testing.T) {
	if err := Prune(t.TempDir(), 2); err != nil {
		t.Fatal(err)
	}
}

func TestLatest(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Latest(dir); err != nil || ok {
		t.Fatalf("empty dir: ok=%v err=%v", ok, err)
	}
	if _, err := Write(dir, []string{"RETURN 1;"}, stamp(t, "20260820-000000")); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(dir, []string{"RETURN 1;"}, stamp(t, "20260824-000000")); err != nil {
		t.Fatal(err)
	}
	in, ok, err := Latest(dir)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if filepath.Base(in.Path) != "imem-20260824-000000.cypherl.gz" {
		t.Fatalf("got %s", in.Path)
	}
}

func TestCleanTempSparesRealBackups(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, []string{"RETURN 1;"}, stamp(t, "20260824-000000")); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".imem-20260825-000000.cypherl.gz.tmp")
	write(t, tmp, "partial")

	if err := CleanTemp(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("temp file survived CleanTemp")
	}
	if n := len(mustList(t, dir)); n != 1 {
		t.Fatalf("got %d backups, want 1", n)
	}
}

func mustList(t *testing.T, dir string) []Info {
	t.Helper()
	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A wiped Memgraph does not dump zero statements: the daemon's EnsureSchema
// recreates indexes and constraints at boot, so the guard has to look for
// nodes rather than for a zero length.
func TestHasData(t *testing.T) {
	schemaOnly := []string{
		"CREATE INDEX ON :`Memory`(`hash`);",
		"CREATE CONSTRAINT ON (u:`Memory`) ASSERT u.`hash` IS UNIQUE;",
	}
	if HasData(schemaOnly) {
		t.Error("schema-only dump must not count as data")
	}
	if HasData(nil) {
		t.Error("empty dump must not count as data")
	}
	withNode := append([]string{"CREATE (:__mg_vertex__:`Memory` {__mg_id__: 1});"}, schemaOnly...)
	if !HasData(withNode) {
		t.Error("dump with a node must count as data")
	}
}

// Entity.key joins project key and name with a NUL, and DUMP DATABASE emits
// that byte raw — which Memgraph's parser rejects on replay. Write must
// escape it so the stored file is valid, replayable Cypher.
func TestWriteEscapesControlBytes(t *testing.T) {
	dir := t.TempDir()
	raw := "CREATE (:__mg_vertex__:`Entity` {`key`: \"/proj\x00memgraph\", `n`: \"a\tb\"});"
	info, err := Write(dir, []string{raw}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d statements, want 1: %q", len(got), got)
	}
	want := "CREATE (:__mg_vertex__:`Entity` {`key`: \"/proj\\u0000memgraph\", `n`: \"a\\u0009b\"});"
	if got[0] != want {
		t.Errorf("\n got %q\nwant %q", got[0], want)
	}
	for i := 0; i < len(got[0]); i++ {
		if c := got[0][i]; c < 0x20 || c == 0x7f {
			t.Fatalf("raw control byte %#x survived escaping at %d", c, i)
		}
	}
}

func TestEscapeControlLeavesCleanTextAlone(t *testing.T) {
	in := "CREATE (:A {t: \"em dash — ellipsis …\"});"
	if got := EscapeControl(in); got != in {
		t.Errorf("got %q, want it unchanged", got)
	}
}
