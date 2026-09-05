package gamecfg

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Reading a file does not destroy it. Only a write does, so `gvm apply --diff`
// over an unchanged instance must not report this file as rewritten.
func TestOwnedLinesReadDoesNotRewrite(t *testing.T) {
	in := readSample(t, "user_jvm_args.txt")
	o := ParseOwnedLines(in)
	if v, ok := o.Get(OwnedLinesKey); !ok || v != "-Xmx4G\n-Xms4G" {
		t.Fatalf("Get = %q, %v", v, ok)
	}
	if !bytes.Equal(o.Bytes(), in) {
		t.Errorf("reading the file changed it: %q", o.Bytes())
	}
}

// This is the one codec that discards unknown content, and the test says so
// out loud: everything a human put in the file is gone after a write. That is
// correct only because the whole file is generated from one variable — merging
// JVM arguments would leave two -Xmx flags and the JVM would take the last.
func TestOwnedLinesWriteReplacesTheEntireFile(t *testing.T) {
	in := []byte("# a comment someone added\n-Xmx2G\n\n-XX:+UseG1GC\n")
	o := ParseOwnedLines(in)
	if err := o.Set(OwnedLinesKey, "-Xmx8G\n-Xms8G"); err != nil {
		t.Fatal(err)
	}
	got := o.String()

	for _, gone := range []string{"a comment someone added", "-Xmx2G", "UseG1GC"} {
		if strings.Contains(got, gone) {
			t.Errorf("owned_lines kept %q; this codec replaces the whole file", gone)
		}
	}
	if !strings.HasPrefix(got, OwnedLinesHeader) {
		t.Errorf("the written file does not carry the header that says gvm owns it:\n%s", got)
	}
	if !strings.Contains(got, "-Xmx8G\n-Xms8G\n") {
		t.Errorf("the new content is wrong:\n%s", got)
	}
}

// A gvm-written file must read back as what was written, or every apply would
// see a difference and rewrite the file again. The commented cases are the ones
// that matter: a Java @argfile takes '#' comments, so an operator who annotates
// jvm_args would otherwise sit on a permanent drift badge and a permanent
// "restart required" for content that is already byte-identical on disk.
func TestOwnedLinesRoundTripIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"plain", "-Xmx4G\n-Xms4G"},
		{"leading comment", "# tuned for the 8G box\n-Xmx6G"},
		{"interior comment", "-Xmx6G\n# why\n-Xms6G"},
		{"only a comment", "# nothing enabled yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := ParseOwnedLines(nil)
			if err := o.Set(OwnedLinesKey, tc.value); err != nil {
				t.Fatal(err)
			}
			first := o.Bytes()

			again := ParseOwnedLines(first)
			v, _ := again.Get(OwnedLinesKey)
			if v != tc.value {
				t.Fatalf("read back %q; want %q", v, tc.value)
			}
			if err := again.Set(OwnedLinesKey, v); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again.Bytes(), first) {
				t.Errorf("writing back what was read produced different bytes:\n%q\n%q", again.Bytes(), first)
			}
		})
	}
}

// A second key would imply the file has structure to merge into, and it does
// not. Refusing at KeySyntax means a manifest that treats it like a normal
// config fails `gvm game validate` rather than applying nothing.
func TestOwnedLinesHasExactlyOneKey(t *testing.T) {
	o := ParseOwnedLines(nil)
	if err := o.Set("jvm_args", "-Xmx1G"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Set of a second key = %v; want ErrKeyNotFound", err)
	}
	if _, ok := o.Get("jvm_args"); ok {
		t.Error("Get answered for a key this format does not have")
	}
	if got := o.Keys(); len(got) != 1 || got[0] != OwnedLinesKey {
		t.Errorf("Keys() = %v; want [%s]", got, OwnedLinesKey)
	}
	if err := (ownedLinesCodec{}).KeySyntax("jvm_args"); err == nil {
		t.Error("KeySyntax accepted a key that would be silently ignored")
	}
}

func TestOwnedLinesDelete(t *testing.T) {
	o := ParseOwnedLines([]byte("-Xmx4G\n"))
	if n := o.Delete(OwnedLinesKey); n != 1 {
		t.Fatalf("Delete reported %d", n)
	}
	if got := o.String(); got != OwnedLinesHeader+"\n" {
		t.Errorf("Delete produced %q", got)
	}
	if n := o.Delete(OwnedLinesKey); n != 0 {
		t.Errorf("deleting an already-empty file reported %d", n)
	}
}

func TestOwnedLinesKeepsTheFilesLineEnding(t *testing.T) {
	o := ParseOwnedLines([]byte("-Xmx2G\r\n"))
	if err := o.Set(OwnedLinesKey, "-Xmx4G\n-Xms4G"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ReplaceAll(o.String(), "\r\n", ""), "\n") {
		t.Errorf("a bare LF was written into a CRLF file: %q", o.String())
	}
}
