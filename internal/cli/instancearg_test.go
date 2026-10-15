package cli

import (
	"strings"
	"testing"
)

func TestExtractInstanceAlias(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantAlias string
		wantRest  []string
		wantErr   string
	}{
		{"none", []string{"pz-main", "--game", "x"}, "", []string{"pz-main", "--game", "x"}, ""},
		{"instance space", []string{"--instance", "pz-main", "--game", "x"}, "pz-main", []string{"--game", "x"}, ""},
		{"instance equals", []string{"--instance=pz-main"}, "pz-main", nil, ""},
		{"name space", []string{"--name", "v1"}, "v1", nil, ""},
		{"single dash", []string{"-instance", "v1", "a"}, "v1", []string{"a"}, ""},
		{"both agree", []string{"--instance", "v1", "--name", "v1"}, "v1", nil, ""},
		{"both differ", []string{"--instance", "a", "--name", "b"}, "", nil, "differ"},
		{"missing value", []string{"--instance"}, "", nil, "needs a value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, alias, err := extractInstanceAlias(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if alias != tc.wantAlias {
				t.Errorf("alias = %q, want %q", alias, tc.wantAlias)
			}
			if strings.Join(rest, " ") != strings.Join(tc.wantRest, " ") {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

// TestInstanceAliasesResolveOnVerbs: --instance and --name are accepted as the
// instance on the verbs that adopted parseWithInstance, and giving both the
// positional and an alias is refused.
func TestInstanceAliasesResolveOnVerbs(t *testing.T) {
	env := parseEnv()

	t.Run("create --instance", func(t *testing.T) {
		opt, err := parseCreateArgs(env, []string{"--instance", "pz-main", "--game", "projectzomboid"})
		if err != nil {
			t.Fatal(err)
		}
		if opt.Name != "pz-main" {
			t.Errorf("name = %q, want pz-main", opt.Name)
		}
	})
	t.Run("create --name", func(t *testing.T) {
		opt, err := parseCreateArgs(env, []string{"--name", "pz-main", "--game", "projectzomboid"})
		if err != nil {
			t.Fatal(err)
		}
		if opt.Name != "pz-main" {
			t.Errorf("name = %q, want pz-main", opt.Name)
		}
	})
	t.Run("apply --instance", func(t *testing.T) {
		opt, err := parseApplyArgs(env, "apply", []string{"--instance", "pz-main"})
		if err != nil {
			t.Fatal(err)
		}
		if opt.Name != "pz-main" {
			t.Errorf("name = %q, want pz-main", opt.Name)
		}
	})
	t.Run("positional and alias both is refused", func(t *testing.T) {
		_, err := parseApplyArgs(env, "apply", []string{"pz-main", "--instance", "pz-main"})
		if err == nil || !strings.Contains(err.Error(), "once") {
			t.Errorf("err = %v, want a 'name it once' refusal", err)
		}
	})
}
