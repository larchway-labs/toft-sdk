package toft

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"time"
)

// A file connection's own access token, for Microsoft's or Google's SDK.
//
// Where an administrator has switched on *Let apps get access tokens* on the
// connection attached to this app, the app calls Graph or Drive directly and
// holds no secret:
//
//	tok, err := toft.AccessToken(ctx, "sharepoint")
//	req.Header.Set("Authorization", "Bearer "+tok.Value)
//
// **The folder is a hint, not a fence.** The token reaches everything the
// connection's grant reaches; Folder only says where this app was meant to work,
// so its code names no site.
//
// There is no Azure adapter here, so the SDK takes no Azure dependency. Five
// lines make one for azidentity's callers:
//
//	type toftCredential struct{ kind string }
//
//	func (c toftCredential) GetToken(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
//		t, err := toft.AccessToken(ctx, c.kind)
//		return azcore.AccessToken{Token: t.Value, ExpiresOn: t.ExpiresAt}, err
//	}

// TokenRefreshMargin is how long before expiry a cached token is replaced.
const TokenRefreshMargin = 5 * time.Minute

// Token is one access token and what it is for.
type Token struct {
	// Value is the bearer token. Never log it.
	Value string
	// ExpiresAt is when the provider stops accepting it.
	ExpiresAt time.Time
	// Resource is the API it is for.
	Resource string
	// Folder is where this app was attached, in the provider's own ids.
	Folder TokenFolder
}

// TokenFolder names a folder. DriveID is Graph's and empty for Google Drive.
type TokenFolder struct {
	DriveID string `json:"drive_id"`
	ItemID  string `json:"item_id"`
	Label   string `json:"label"`
}

// String never prints the value: a token in a log line is held by whoever
// reads the log.
func (t Token) String() string {
	return "Token(" + t.Resource + ", expires " + t.ExpiresAt.Format(time.RFC3339) + ")"
}

var tokenCache = struct {
	sync.Mutex
	m map[string]Token
}{m: map[string]Token{}}

// AccessToken is the attached kind's token: "sharepoint", "onedrive" or
// "gdrive". It is cached per kind and fetched again [TokenRefreshMargin] before
// it expires, so calling it before every request costs one request to Toft an
// hour. A refusal is an [*Error] carrying the platform's words.
func AccessToken(ctx context.Context, kind string) (Token, error) {
	tokenCache.Lock()
	cached, ok := tokenCache.m[kind]
	tokenCache.Unlock()
	if ok && time.Until(cached.ExpiresAt) > TokenRefreshMargin {
		return cached, nil
	}

	t, err := client()
	if err != nil {
		return Token{}, err
	}
	raw, err := t.postJSON(ctx, "/api/v1/app/tokens/"+url.PathEscape(kind), nil, "", nil)
	if err != nil {
		return Token{}, err
	}
	var wire struct {
		AccessToken string      `json:"access_token"`
		ExpiresAt   time.Time   `json:"expires_at"`
		Resource    string      `json:"resource"`
		Folder      TokenFolder `json:"folder"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Token{}, err
	}
	tok := Token{Value: wire.AccessToken, ExpiresAt: wire.ExpiresAt, Resource: wire.Resource,
		Folder: wire.Folder}

	tokenCache.Lock()
	tokenCache.m[kind] = tok
	tokenCache.Unlock()
	return tok, nil
}

// resetTokenCache forgets every cached token. For tests.
func resetTokenCache() {
	tokenCache.Lock()
	tokenCache.m = map[string]Token{}
	tokenCache.Unlock()
}
