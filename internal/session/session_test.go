package session

import (
	"reflect"
	"testing"
)

// EffectiveCwd and substituteTokens are pure logic, unlike the rest of
// this package (Connect/NewSession/Prompt need a live subprocess to
// exercise meaningfully — see CLAUDE.md) — safe to unit test directly.

func TestSpec_EffectiveCwd_DefaultsToHostCwd(t *testing.T) {
	s := Spec{Name: "claude"}
	if got := s.EffectiveCwd(`C:\chorus`); got != `C:\chorus` {
		t.Errorf("EffectiveCwd() = %q, want host cwd unchanged", got)
	}
}

func TestSpec_EffectiveCwd_UsesWorkDirWhenSet(t *testing.T) {
	s := Spec{Name: "opencode", WorkDir: "/workspace"}
	if got := s.EffectiveCwd(`C:\chorus`); got != "/workspace" {
		t.Errorf("EffectiveCwd() = %q, want /workspace", got)
	}
}

func TestSubstituteTokens_ReplacesCwdToken(t *testing.T) {
	got := substituteTokens([]string{"run", "-v", "{{CWD}}:/workspace", "image"}, `C:\chorus`)
	want := []string{"run", "-v", `C:\chorus:/workspace`, "image"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("substituteTokens() = %v, want %v", got, want)
	}
}

func TestSubstituteTokens_NoTokenLeavesArgsUnchanged(t *testing.T) {
	got := substituteTokens([]string{"acp"}, `C:\chorus`)
	want := []string{"acp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("substituteTokens() = %v, want %v", got, want)
	}
}

func TestSubstituteTokens_ReplacesEnvToken(t *testing.T) {
	t.Setenv("CHORUS_TEST_GCLOUD_DIR", `C:\Users\someone\AppData\Roaming`)
	got := substituteTokens([]string{"-v", `{{ENV:CHORUS_TEST_GCLOUD_DIR}}\gcloud:/home/node/.config/gcloud:ro`}, `C:\chorus`)
	want := []string{`-v`, `C:\Users\someone\AppData\Roaming\gcloud:/home/node/.config/gcloud:ro`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("substituteTokens() = %v, want %v", got, want)
	}
}

func TestSubstituteTokens_UnsetEnvTokenSubstitutesEmpty(t *testing.T) {
	got := substituteTokens([]string{"{{ENV:CHORUS_TEST_DEFINITELY_UNSET}}/x"}, `C:\chorus`)
	want := []string{"/x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("substituteTokens() = %v, want %v", got, want)
	}
}

func TestSubstituteTokens_CombinesCwdAndEnvTokensInOneArg(t *testing.T) {
	t.Setenv("CHORUS_TEST_HOME", "/home/someone")
	got := substituteTokens([]string{"-v", "{{ENV:CHORUS_TEST_HOME}}/.config/gcloud:{{CWD}}/mnt"}, "/workspace")
	want := []string{"-v", "/home/someone/.config/gcloud:/workspace/mnt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("substituteTokens() = %v, want %v", got, want)
	}
}
