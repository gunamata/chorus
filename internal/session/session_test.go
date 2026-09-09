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

// Cwd() is the value sub-session creators (the LLM router's decision
// sub-session, the delegate hub) must use instead of the raw host cwd, so
// a sandboxed agent gets its in-container mount point — Connect wires it
// from spec.EffectiveCwd(cwd). The wiring in Connect itself needs a live
// subprocess (see above); the accessor contract is pure logic.
func TestConnection_Cwd_ReturnsEffectiveCwd(t *testing.T) {
	c := &Connection{cwd: "/workspace"}
	if got := c.Cwd(); got != "/workspace" {
		t.Errorf("Cwd() = %q, want /workspace", got)
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

func TestEnvLines_EmptyMapReturnsNil(t *testing.T) {
	if got := envLines(nil, "/workspace"); got != nil {
		t.Errorf("envLines(nil, ...) = %v, want nil", got)
	}
	if got := envLines(map[string]string{}, "/workspace"); got != nil {
		t.Errorf("envLines({}, ...) = %v, want nil", got)
	}
}

func TestEnvLines_FormatsAndSubstitutesTokens(t *testing.T) {
	t.Setenv("CHORUS_TEST_HEADROOM_URL", "http://127.0.0.1:8787")
	got := envLines(map[string]string{"ANTHROPIC_BASE_URL": "{{ENV:CHORUS_TEST_HEADROOM_URL}}"}, "/workspace")
	want := []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:8787"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envLines() = %v, want %v", got, want)
	}
}

func TestEnvLines_CwdTokenInValue(t *testing.T) {
	got := envLines(map[string]string{"PROJECT_DIR": "{{CWD}}/sub"}, "/workspace")
	want := []string{"PROJECT_DIR=/workspace/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envLines() = %v, want %v", got, want)
	}
}
