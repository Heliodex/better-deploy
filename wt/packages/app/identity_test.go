package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIdentitySignatureVector(t *testing.T) {
	if got := identitySignature("identity-secret", "usr_1", "ada@example.com", "Ada", "admins,beta"); got != "4eb999754e66af1d072d352fd3504b1fb6f404e7bfa414a0aec935fd5eebea49" {
		t.Errorf("identity signature = %s", got)
	}
}

func TestUserFromRequest(t *testing.T) {
	t.Setenv(identitySecretEnv, "identity-secret")

	groups := "admins,beta"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(identityUserIDHeader, "usr_1")
	req.Header.Set(identityUserEmailHeader, "ada@example.com")
	req.Header.Set(identityUserNameHeader, "Ada")
	req.Header.Set(identityUserGroupsHeader, groups)
	req.Header.Set(identityUserSignatureHeader, identitySignature("identity-secret", "usr_1", "ada@example.com", "Ada", groups))

	user, ok := UserFromRequest(req)
	if !ok || user.ID != "usr_1" || user.Email != "ada@example.com" || user.Name != "Ada" {
		t.Fatalf("UserFromRequest = %+v, %v", user, ok)
	}

	if len(user.Groups) != 2 || user.Groups[0] != "admins" || user.Groups[1] != "beta" {
		t.Errorf("groups = %v", user.Groups)
	}

	req.Header.Set(identityUserNameHeader, "Mallory")

	if _, ok := UserFromRequest(req); ok {
		t.Error("modified header was accepted")
	}

	req.Header.Del(identityUserIDHeader)

	if _, ok := UserFromRequest(req); ok {
		t.Error("missing id was accepted")
	}
}

func TestUserFromRequestWithoutSecret(t *testing.T) {
	t.Setenv(identitySecretEnv, "")

	if _, ok := UserFromRequest(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
		t.Error("request without a secret was accepted")
	}
}
