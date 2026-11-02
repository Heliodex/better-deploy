package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const (
	// IdentityPrefix is the reserved app-host path for platform login, logout, me and password.
	IdentityPrefix              = "/_wt/identity"
	identitySecretEnv           = "WT_IDENTITY_SECRET" //nolint:gosec // Environment variable name, not a credential.
	identityUserIDHeader        = "X-WT-User-Id"
	identityUserEmailHeader     = "X-WT-User-Email"
	identityUserNameHeader      = "X-WT-User-Name"
	identityUserGroupsHeader    = "X-WT-User-Groups"
	identityUserSignatureHeader = "X-WT-User-Signature"
)

// User is the platform user the supervisor attached to this request.
type User struct {
	ID     string   `json:"id"`
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
}

// identitySignature computes the supervisor's HMAC over the identity headers. The field order and separators must match the supervisor.
func identitySignature(secret, id, email, name, groups string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%s\n%s", id, email, name, groups)

	return hex.EncodeToString(mac.Sum(nil))
}

// UserFromRequest returns the signed platform user when the app has identity: "platform".
// It rejects client-supplied headers without a valid supervisor signature.
func UserFromRequest(r *http.Request) (User, bool) {
	secret := os.Getenv(identitySecretEnv)
	if secret == "" {
		return User{}, false
	}

	id := r.Header.Get(identityUserIDHeader)
	email := r.Header.Get(identityUserEmailHeader)
	name := r.Header.Get(identityUserNameHeader)
	groupsHeader := r.Header.Get(identityUserGroupsHeader)

	signature := r.Header.Get(identityUserSignatureHeader)
	if id == "" || len(signature) != sha256.Size*2 {
		return User{}, false
	}

	want, err := hex.DecodeString(identitySignature(secret, id, email, name, groupsHeader))
	if err != nil {
		return User{}, false
	}

	got, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal(got, want) {
		return User{}, false
	}

	var groups []string

	for group := range strings.SplitSeq(groupsHeader, ",") {
		group = strings.TrimSpace(group)
		if group != "" {
			groups = append(groups, group)
		}
	}

	return User{ID: id, Email: email, Name: name, Groups: groups}, true
}
