package platform

import "testing"

func TestParsePortRange(t *testing.T) {
	// The kernel writes the two numbers separated by a tab.
	got, err := ParsePortRange([]byte("32768\t60999\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != (PortRange{Low: 32768, High: 60999}) {
		t.Fatalf("got %v", got)
	}
	if got != DefaultEphemeral {
		t.Fatalf("the kernel default parsed as %v, not DefaultEphemeral %v", got, DefaultEphemeral)
	}

	// A hardened host narrows the range so servers can use the space below it.
	if got, err = ParsePortRange([]byte("49152 65535")); err != nil || got.Low != 49152 || got.High != 65535 {
		t.Fatalf("got %v, %v", got, err)
	}
	// A single-port range is legal and worth getting right: it is what an
	// operator sets when they want the allocator to have everything else.
	if got, err = ParsePortRange([]byte("60000 60000\n")); err != nil || !got.Contains(60000) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestParsePortRangeRejectsNonsense(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"one field":      "32768\n",
		"three fields":   "32768 60999 1\n",
		"low not a port": "x 60999\n",
		"hi not a port":  "32768 y\n",
		"inverted":       "60999 32768\n",
		"zero low":       "0 60999\n",
		"over 65535":     "32768 70000\n",
		"negative":       "-1 60999\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := ParsePortRange([]byte(in)); err == nil {
				t.Fatalf("accepted %q as %v", in, got)
			}
		})
	}
}

func TestPortRangeContains(t *testing.T) {
	r := PortRange{Low: 32768, High: 60999}
	for _, p := range []int{32768, 40000, 60999} {
		if !r.Contains(p) {
			t.Errorf("%d should be inside %v", p, r)
		}
	}
	for _, p := range []int{0, 1024, 16261, 32767, 61000, 65535, 70000, -1} {
		if r.Contains(p) {
			t.Errorf("%d should be outside %v", p, r)
		}
	}
	// A failed read must not silently make every port look non-ephemeral OR
	// every port look ephemeral; the zero value contains nothing and the
	// caller substitutes DefaultEphemeral deliberately.
	var zero PortRange
	if zero.Valid() || zero.Contains(40000) {
		t.Fatal("the zero PortRange behaves like a real range")
	}
	if !DefaultEphemeral.Contains(40000) {
		t.Fatal("DefaultEphemeral does not contain a port the kernel would assign")
	}
	// The ports the six shipped manifests default to must sit outside the
	// kernel default, or every allocation would start by shifting.
	for _, p := range []int{16261, 16262, 27015, 2456, 25565, 26900} {
		if DefaultEphemeral.Contains(p) {
			t.Errorf("shipped default port %d falls inside the ephemeral range", p)
		}
	}
}

func TestPortRangeString(t *testing.T) {
	if got := (PortRange{Low: 32768, High: 60999}).String(); got != "32768-60999" {
		t.Fatalf("got %q", got)
	}
}
