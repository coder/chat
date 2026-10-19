package slackapi_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/coder/chat/adapters/slack/slackapi"
	"github.com/coder/chat/adapters/slack/slackapi/slackapitest"
)

const appsConfigToken = "xoxe.xoxp-config"

// appsOnlyCall returns the only request to method on srv and checks that it
// is form encoded with the app configuration token.
func appsOnlyCall(t *testing.T, srv *slackapitest.Server, method string) slackapitest.Call {
	t.Helper()
	calls := srv.Calls(method)
	if len(calls) != 1 {
		t.Fatalf("%s calls = %d, want 1", method, len(calls))
	}
	call := calls[0]
	if got, want := call.Header.Get("Authorization"), "Bearer "+appsConfigToken; got != want {
		t.Errorf("%s Authorization = %q, want %q", method, got, want)
	}
	if got := call.Header.Get("Content-Type"); got != methodsFormType {
		t.Errorf("%s Content-Type = %q, want %q", method, got, methodsFormType)
	}
	return call
}

func TestCreateApp(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("apps.manifest.create", json.RawMessage(`{
		"ok": true,
		"app_id": "A0APP",
		"credentials": {
			"client_id": "123.456",
			"client_secret": "client-secret",
			"verification_token": "verification-token",
			"signing_secret": "signing-secret"
		},
		"oauth_authorize_url": "https://slack.com/oauth/v2/authorize?client_id=123.456&scope=chat:write"
	}`))

	resp, err := methodsNewClient(srv).WithToken(appsConfigToken).CreateApp(t.Context(), slackapi.CreateAppRequest{
		Manifest: formatsManifest(),
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	call := appsOnlyCall(t, srv, "apps.manifest.create")
	if len(call.Form) != 1 || len(call.Form["manifest"]) != 1 {
		t.Fatalf("form = %v, want only manifest", call.Form)
	}
	formatsAssertSameJSON(t, []byte(call.Form.Get("manifest")), []byte(formatsManifestJSON))
	methodsCheckEqual(t, "response", resp, &slackapi.CreateAppResponse{
		AppID: "A0APP",
		Credentials: slackapi.AppCredentials{
			ClientID:      "123.456",
			ClientSecret:  "client-secret",
			SigningSecret: "signing-secret",
		},
	})
}

func TestDeleteApp(t *testing.T) {
	t.Parallel()

	srv := slackapitest.NewServer(t)
	srv.Respond("apps.manifest.delete", json.RawMessage(`{"ok":true}`))

	err := methodsNewClient(srv).WithToken(appsConfigToken).DeleteApp(t.Context(), slackapi.DeleteAppRequest{AppID: "A0APP"})
	if err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	call := appsOnlyCall(t, srv, "apps.manifest.delete")
	methodsCheckEqual(t, "form", call.Form, url.Values{"app_id": {"A0APP"}})
}
