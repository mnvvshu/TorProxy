package logger

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLevelFiltering(t *testing.T) {
	l := NewNop()
	l.SetLevel(LevelWarn)
	l.Info("hidden")
	l.Warn("shown")
	entries := l.Recent(0, 0)
	if len(entries) != 1 || entries[0].Message != "shown" {
		t.Fatalf("expected only the warn entry, got %+v", entries)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{"debug": LevelDebug, "INFO": LevelInfo, "warning": LevelWarn, "error": LevelError, "bogus": LevelInfo}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestChildLoggersShareCore(t *testing.T) {
	l := NewNop()
	child := l.WithInstance(7)
	child.Info("hello from child")
	entries := l.Recent(0, 0)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Component != "test:instance-7" {
		t.Errorf("unexpected component %q", entries[0].Component)
	}
}

func TestRecentAfterSeq(t *testing.T) {
	l := NewNop()
	for i := 0; i < 5; i++ {
		l.Info("msg %d", i)
	}
	all := l.Recent(0, 0)
	newer := l.Recent(0, all[2].Seq)
	if len(newer) != 2 {
		t.Fatalf("expected 2 entries after seq %d, got %d", all[2].Seq, len(newer))
	}
	last2 := l.Recent(2, 0)
	if len(last2) != 2 || last2[1].Message != "msg 4" {
		t.Fatalf("expected last two entries, got %+v", last2)
	}
}

func TestRingBufferWraps(t *testing.T) {
	l := NewNop()
	for i := 0; i < defaultRingSize+50; i++ {
		l.Info("m%d", i)
	}
	entries := l.Recent(0, 0)
	if len(entries) != defaultRingSize {
		t.Fatalf("expected %d entries, got %d", defaultRingSize, len(entries))
	}
	if entries[0].Message != "m50" {
		t.Errorf("expected oldest retained entry m50, got %s", entries[0].Message)
	}
}

func TestSubscribe(t *testing.T) {
	l := NewNop()
	ch := l.Subscribe()
	l.Error("boom")
	e := <-ch
	if e.Message != "boom" || e.Level != "ERROR" {
		t.Errorf("unexpected entry %+v", e)
	}
	l.Unsubscribe(ch)
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after unsubscribe")
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, os.ErrInvalid }

func TestFileWrittenEvenIfConsoleFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	l, err := New(path, "app", 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	l.SetConsole(failingWriter{})
	l.Info("persist me")
	l.Close()

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "persist me") {
		t.Errorf("log file missing entry; content=%q", data)
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	l, err := New(path, "app", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	l.SetConsole(nil)
	big := strings.Repeat("x", 64*1024)
	for i := 0; i < 80; i++ { // ~5 MB => several rotations
		l.Info("%s", big)
	}
	child := l.WithInstance(1)
	child.Info("after rotation")
	l.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "app.*.log"))
	if len(files) == 0 {
		t.Fatal("expected rotated files")
	}
	if len(files) > 2 {
		t.Errorf("expected at most maxFiles-1 (2) rotated files, got %d", len(files))
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("after rotation")) {
		t.Error("child logger output lost after rotation")
	}
}

func TestRedaction(t *testing.T) {
	cases := []struct{ in, mustNot string }{
		{"AUTHENTICATE 0123456789abcdef0123456789abcdef", "0123456789abcdef"},
		{"cookie=" + strings.Repeat("ab", 32), strings.Repeat("ab", 32)},
		{"proxy socks5://alice:s3cret@127.0.0.1:9050", "s3cret"},
	}
	for _, c := range cases {
		got := redactSensitive(c.in)
		if strings.Contains(got, c.mustNot) {
			t.Errorf("redactSensitive(%q) = %q still contains secret", c.in, got)
		}
	}
	if got := redactSensitive("port 9050 ready"); got != "port 9050 ready" {
		t.Errorf("benign message altered: %q", got)
	}
}
