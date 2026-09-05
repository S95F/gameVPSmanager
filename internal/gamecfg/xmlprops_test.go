package gamecfg

import (
	"errors"
	"strings"
	"testing"
)

// The shipped serverconfig.xml disables several options by wrapping them in a
// comment. Treating one as live lets a Set write inside the comment and report
// success while the setting stays off.
func TestXMLCommentedOutPropertiesAreInvisible(t *testing.T) {
	x := ParseXMLProps(readSample(t, "serverconfig.xml"))

	for _, name := range []string{"ServerIP", "TelnetFailedLoginLimit"} {
		if _, ok := x.Get(name); ok {
			t.Errorf("Get(%q) found a property that is inside <!-- -->", name)
		}
		if err := x.Set(name, "x"); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("Set(%q) = %v; a commented-out property must not be writable", name, err)
		}
	}
	for _, k := range x.Keys() {
		if k == "ServerIP" {
			t.Error("Keys() listed a commented-out property")
		}
	}
}

// <propertyGroup> shares a prefix with <property>. Matching on the prefix
// alone would let a group element's attributes be read as a property.
func TestXMLElementNameMustMatchExactly(t *testing.T) {
	in := `<S><propertyGroup name="A" value="1"/><property name="B" value="2"/></S>`
	x := ParseXMLProps([]byte(in))
	if got := x.Keys(); len(got) != 1 || got[0] != "B" {
		t.Fatalf("Keys() = %v; want just [B]", got)
	}
}

func TestXMLSetPreservesIndentationAndTrailingComment(t *testing.T) {
	x := ParseXMLProps(readSample(t, "serverconfig.xml"))
	if err := x.Set("GameDifficulty", "4"); err != nil {
		t.Fatal(err)
	}
	want := "\t<property name=\"GameDifficulty\"\t\t\t\tvalue=\"4\"/>\t\t\t\t\t\t\t\t<!-- 0 - 5, 0=easiest, 5=hardest -->"
	if !strings.Contains(x.String(), want) {
		t.Errorf("the tab alignment or the trailing comment did not survive\nwanted line: %q", want)
	}
}

func TestXMLEscapesAndUnescapes(t *testing.T) {
	tests := []struct {
		value    string
		wantFile string
	}{
		{`Bob's & Alice's`, `Bob&apos;s &amp; Alice&apos;s`},
		{`say "hi"`, `say &quot;hi&quot;`},
		{`a<b`, `a&lt;b`},
		{"two\nlines", "two&#10;lines"},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			x := ParseXMLProps([]byte(`<S><property name="A" value=""/></S>`))
			if err := x.Set("A", tt.value); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(x.String(), tt.wantFile) {
				t.Fatalf("wrote %q; want it to contain %q", x.String(), tt.wantFile)
			}
			if got, _ := ParseXMLProps(x.Bytes()).Get("A"); got != tt.value {
				t.Errorf("read back %q; want %q", got, tt.value)
			}
		})
	}
}

func TestXMLSingleQuotedAttributesAreRead(t *testing.T) {
	x := ParseXMLProps([]byte(`<S><property name='A' value='1'/></S>`))
	if v, ok := x.Get("A"); !ok || v != "1" {
		t.Errorf("Get = %q, %v; single-quoted attributes are valid XML", v, ok)
	}
}

// A <property> with no value attribute is a malformed line in a file the game
// wrote. Inventing the attribute hides that from whoever has to fix it.
func TestXMLPropertyWithoutAValueAttributeIsRefused(t *testing.T) {
	x := ParseXMLProps([]byte(`<S><property name="A"/></S>`))
	err := x.Set("A", "1")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Set = %v; want ErrUnsupported", err)
	}
	if strings.Contains(x.String(), "value") {
		t.Error("Set invented a value attribute after refusing")
	}
}

func TestXMLDeleteTakesTheLineAndItsComment(t *testing.T) {
	in := "<S>\n\t<property name=\"A\" value=\"1\"/>\t<!-- about A -->\n\t<property name=\"B\" value=\"2\"/>\n</S>\n"
	x := ParseXMLProps([]byte(in))
	if n := x.Delete("A"); n != 1 {
		t.Fatalf("Delete removed %d", n)
	}
	want := "<S>\n\t<property name=\"B\" value=\"2\"/>\n</S>\n"
	if got := x.String(); got != want {
		t.Errorf("Delete produced\n%q\nwant\n%q", got, want)
	}
}

func TestXMLUnknownKeyIsNotFound(t *testing.T) {
	x := ParseXMLProps(readSample(t, "serverconfig.xml"))
	if err := x.Set("NoSuchProperty", "1"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Set of an unknown property = %v; want ErrKeyNotFound", err)
	}
}

func TestXMLNumericCharacterReferences(t *testing.T) {
	x := ParseXMLProps([]byte(`<S><property name="A" value="a&#65;b&#x42;"/></S>`))
	if v, _ := x.Get("A"); v != "aAbB" {
		t.Errorf("Get = %q; want %q", v, "aAbB")
	}
}
