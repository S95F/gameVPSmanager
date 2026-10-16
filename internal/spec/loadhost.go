package spec

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// unknownFieldRe matches yaml.v3's KnownFields error lines, e.g.
// "line 5: field reserved_port not found in type spec.FirewallSpec".
var unknownFieldRe = regexp.MustCompile(`line (\d+): field (\S+) not found`)

// LoadHost parses /etc/gvm/gvm.yaml onto the shipped defaults and validates it.
//
// The merge is the point: a file that sets one key keeps every other default,
// so `firewall: {}` is a no-op and an operator never has to restate the whole
// config to change one line. Unknown keys are collected — not fatal on the
// first one — each with a did-you-mean, because the mistake that costs an
// operator an afternoon is a typo'd key that plain YAML would ignore in
// silence. The returned config is always usable (defaults where the file said
// nothing); the Diags say what, if anything, is wrong with it.
func LoadHost(file string, data []byte) (*HostConfig, Diags) {
	h := DefaultHostConfig()
	var diags Diags

	if err := yaml.Unmarshal(data, &h); err != nil {
		diags = append(diags, Diag{File: file, Sev: SevError, Msg: fmt.Sprintf("not valid YAML: %v", err)})
		return &h, diags
	}

	// A second, strict pass whose only job is to surface unknown keys; its
	// decoded value is discarded. KnownFields collects every unknown field into
	// one TypeError rather than stopping at the first.
	var probe HostConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&probe); err != nil {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			for _, m := range te.Errors {
				sub := unknownFieldRe.FindStringSubmatch(m)
				if sub == nil {
					continue
				}
				line, _ := strconv.Atoi(sub[1])
				key := sub[2]
				msg := fmt.Sprintf("unknown key %s", key)
				if s := nearestHostKey(key); s != "" {
					msg += fmt.Sprintf(" — did you mean %s?", s)
				}
				diags = append(diags, Diag{File: file, Line: line, Sev: SevError, Msg: msg})
			}
		}
		// A non-TypeError here (the lenient pass already caught real syntax
		// errors) is not worth a second, confusing message.
	}

	diags = append(diags, ValidateHost(file, &h)...)
	return &h, diags
}

// ValidateHost checks the enums, CIDRs and port ranges the rest of gvm trusts.
// It is separate from LoadHost so a caller holding an already-parsed config
// (the dashboard, a test) can validate without re-reading the file.
func ValidateHost(file string, h *HostConfig) Diags {
	var ds Diags
	add := func(path, msg, fix string) {
		d := Diag{File: file, Path: path, Msg: msg, Sev: SevError}
		if fix != "" {
			d = d.WithFix("%s", fix)
		}
		ds = append(ds, d)
	}

	switch h.Spec.Firewall.Backend {
	case "ufw", "none", "":
	default:
		add("spec.firewall.backend", fmt.Sprintf("%q is not a firewall backend", h.Spec.Firewall.Backend),
			"use ufw or none")
	}
	switch h.Spec.Edge.Mode {
	case "caddy_managed", "caddy_snippet", "proxy_external", "none", "":
	default:
		add("spec.edge.mode", fmt.Sprintf("%q is not an edge mode", h.Spec.Edge.Mode),
			"use caddy_managed, caddy_snippet, proxy_external or none")
	}
	switch h.Spec.Edge.TLS.Mode {
	case "auto", "acme_http", "acme_dns", "internal", "external", "":
	default:
		add("spec.edge.tls.mode", fmt.Sprintf("%q is not a tls mode", h.Spec.Edge.TLS.Mode),
			"use auto, acme_http, acme_dns, internal or external")
	}

	for i, c := range h.Spec.Firewall.AdminCIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			add(fmt.Sprintf("spec.firewall.admin_cidrs[%d]", i),
				fmt.Sprintf("%q is not a CIDR", c), "use address/prefix, e.g. 10.0.0.0/8")
		}
	}
	for i, c := range h.Spec.Edge.TrustedProxies {
		if _, _, err := net.ParseCIDR(c); err != nil {
			add(fmt.Sprintf("spec.edge.trusted_proxies[%d]", i),
				fmt.Sprintf("%q is not a CIDR", c), "use address/prefix, e.g. 10.0.0.0/8")
		}
	}
	for i, p := range h.Spec.Firewall.ReservedPorts {
		if p < 1 || p > 65535 {
			add(fmt.Sprintf("spec.firewall.reserved_ports[%d]", i),
				fmt.Sprintf("%d is not a port (1–65535)", p), "")
		}
	}
	for i, p := range h.Spec.Firewall.SSH.Ports {
		if p < 1 || p > 65535 {
			add(fmt.Sprintf("spec.firewall.ssh.ports[%d]", i),
				fmt.Sprintf("%d is not a port (1–65535)", p), "")
		}
	}
	return ds
}

// hostKeySet is every yaml key the host config defines, gathered once by
// reflection so the did-you-mean suggestions cannot drift from the struct.
var hostKeySet = collectHostKeys()

func collectHostKeys() []string {
	set := map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if name != "" && name != "-" {
				set[name] = true
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(HostConfig{}))
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// nearestHostKey returns the closest real key within a small edit distance, or
// "" when nothing is close enough (so a wholly wrong key gets no misleading
// suggestion). It reuses the manifest did-you-mean machinery.
func nearestHostKey(key string) string {
	return nearestName(key, hostKeySet)
}
