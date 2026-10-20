package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	kitnotify "github.com/marsadhq/kit/notify"
	"github.com/marsadhq/kit/secrets"
	"github.com/marsadhq/tend/internal/cli"
	"github.com/marsadhq/tend/internal/notify"
)

// TestChannelAddNtfyAndPushover: `tend channel add` takes the ntfy and
// Pushover types with their JSON config, stores the config encrypted under
// the master key like any other channel's, and the stored config builds the
// provider the delivery worker will use.
func TestChannelAddNtfyAndPushover(t *testing.T) {
	cfg := keyedConfig(t)
	ctx := context.Background()
	cases := []struct {
		name, typ, config string
		kind              kitnotify.ChannelType
	}{
		{"phone", "ntfy", `{"url":"https://ntfy.sh/tend-SECRETTOPIC","token":"tk_SECRET","priority":4,"tags":["warning"]}`, kitnotify.Ntfy},
		{"pager", "pushover", `{"token":"aSECRETAPPTOKEN","user":"uSECRETUSER","priority":2,"retry":60,"expire":3600}`, kitnotify.Pushover},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		if err := cli.Run(ctx, cfg, []string{"channel", "add", "-name", c.name, "-type", c.typ},
			strings.NewReader(c.config), &stdout, &stderr); err != nil {
			t.Fatalf("channel add -type %s: %v\nstderr: %s", c.typ, err, stderr.String())
		}
		if strings.Contains(stdout.String(), "SECRET") {
			t.Errorf("channel add -type %s echoed its config: %q", c.typ, stdout.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if err := cli.Run(ctx, cfg, []string{"channel", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("channel list: %v", err)
	}
	for _, c := range cases {
		if !strings.Contains(stdout.String(), c.name) || !strings.Contains(stdout.String(), c.typ) {
			t.Errorf("channel list does not show %s (%s):\n%s", c.name, c.typ, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "SECRET") {
		t.Errorf("channel list printed a config value:\n%s", stdout.String())
	}

	st := openTestStore(t, cfg.DSN)
	org, err := st.BootstrapDefaultOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(cfg.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	channels, err := st.ListChannels(ctx, org.ID)
	if err != nil || len(channels) != len(cases) {
		t.Fatalf("ListChannels = %v, %v; want %d", channels, err, len(cases))
	}
	for _, ch := range channels {
		var want *struct {
			name, typ, config string
			kind              kitnotify.ChannelType
		}
		for i := range cases {
			if cases[i].name == ch.Name {
				want = &cases[i]
			}
		}
		if want == nil || ch.Kind != want.kind {
			t.Fatalf("channel %q of kind %q not expected", ch.Name, ch.Kind)
		}
		_, blob, err := st.GetChannel(ctx, org.ID, ch.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(blob, "SECRET") {
			t.Errorf("channel %q config is stored in plaintext", ch.Name)
		}
		_, plain, err := notify.GetChannelDecrypted(ctx, st, box, org.ID, ch.ID)
		if err != nil || string(plain) != want.config {
			t.Errorf("channel %q decrypts to %s, %v; want %s", ch.Name, plain, err, want.config)
		}
		if _, err := kitnotify.BuildProvider(ch.Kind, plain); err != nil {
			t.Errorf("BuildProvider(%s): %v", ch.Kind, err)
		}
	}
}

// TestChannelAddUnknownTypeNamesTheValidOnes: an unknown -type is refused
// with every accepted type named.
func TestChannelAddUnknownTypeNamesTheValidOnes(t *testing.T) {
	cfg := keyedConfig(t)
	var stdout, stderr bytes.Buffer
	err := cli.Run(context.Background(), cfg, []string{"channel", "add", "-name", "x", "-type", "carrier-pigeon"},
		strings.NewReader(`{}`), &stdout, &stderr)
	if err == nil {
		t.Fatal("channel add accepted an unknown type")
	}
	const want = "webhook, slack, discord, smtp, telegram, ntfy, or pushover"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not list %q", err, want)
	}
}
