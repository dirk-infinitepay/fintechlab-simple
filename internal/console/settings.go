package console

import (
	"os"
	"strings"
)

// The Configuration view.
//
// This lab's behaviour lives almost entirely in environment variables set
// in compose.yml plus two config files. Finding the one knob you want
// means grepping a 400-line compose file, so the lab API keeps a curated
// index of the ones that actually change what happens.
//
// What is shown live and what is only described is kept strictly apart.
// The lab API can read its own environment and the files mounted into it;
// it cannot read another container's environment, and claiming otherwise
// would be worse than not showing it -- an operator who trusts a stale
// value here will debug the wrong thing for an hour.

// Setting is one documented knob.
type Setting struct {
	Key     string `json:"key"`
	Service string `json:"service"`
	Purpose string `json:"purpose"`
	// Configured is the value compose.yml ships, for orientation. It is
	// labelled as such in the UI and is never presented as live.
	Configured string `json:"configured"`
	// Live is set only for the lab API's own environment.
	Live string `json:"live,omitempty"`
}

// SettingGroup is a titled block of settings.
type SettingGroup struct {
	Title    string    `json:"title"`
	Note     string    `json:"note,omitempty"`
	Settings []Setting `json:"settings"`
}

// SettingGroups returns the documented configuration index.
func SettingGroups() []SettingGroup {
	groups := []SettingGroup{
		{
			Title: "Worldline -- the settlement cycle",
			Note:  "Set on the worldline service in compose.yml. Changing any of these needs a restart of that service.",
			Settings: []Setting{
				{Key: "WORLDLINE_FILE_SOURCE", Service: "worldline", Configured: "generate",
					Purpose: "\"generate\" builds the settlement file from the transactions the acquirer holds. \"fixture\" serves a real example file from WORLDLINE_FIXTURE_DIR byte-for-byte, which is how you test a parser against the genuine article."},
				{Key: "WORLDLINE_MORNING_FILE_AT", Service: "worldline", Configured: "08:00",
					Purpose: "Start of the morning (ER) delivery window."},
				{Key: "WORLDLINE_MORNING_FILE_WINDOW", Service: "worldline", Configured: "2h",
					Purpose: "The morning file lands somewhere in this window, jittered deterministically, so a consumer cannot come to depend on an exact second."},
				{Key: "WORLDLINE_AFTERNOON_FILE_AT", Service: "worldline", Configured: "15:30",
					Purpose: "The afternoon (AR) confirmation file. Retained, never reprocessed, and it moves no money."},
				{Key: "WORLDLINE_SETTLEMENT_DELAY", Service: "worldline", Configured: "24h",
					Purpose: "T+1. A cycle run today settles yesterday's transactions -- which is why seeding trading dated today then running a cycle produces nothing."},
				{Key: "WORLDLINE_SETTLEMENT_ACCOUNT", Service: "worldline", Configured: "GB00SIM0000000000001",
					Purpose: "The safeguarding account named in the file's Settlement section, and where the lump sum is paid."},
				{Key: "WORLDLINE_SFTP_PASSWORD", Service: "worldline", Configured: "(a fake lab credential)",
					Purpose: "SFTP password auth. Public-key auth runs alongside it whenever WORLDLINE_SFTP_AUTHORIZED_KEY is set."},
			},
		},
		{
			Title: "Settlement -- taking delivery",
			Note:  "The platform stand-in's whole configuration surface for the Worldline integration. Point these at a real host and the same code runs unchanged.",
			Settings: []Setting{
				{Key: "WORLDLINE_SFTP_HOST", Service: "settlement", Configured: "worldline",
					Purpose: "Where to dial. This is the one variable a UAT or production swap changes."},
				{Key: "WORLDLINE_SFTP_PRIVATE_KEY_PATH", Service: "settlement", Configured: "(unset -- password auth)",
					Purpose: "Set this against a key-authenticated account. The client offers a key first and falls back to the password, so one configuration works on both."},
				{Key: "WORLDLINE_SFTP_KNOWN_HOST_PATH", Service: "settlement", Configured: "/keys/wlsftp-keys/host_key.pub",
					Purpose: "Pins the acquirer's SSH host key. Unset it and the client accepts whatever answers on the port."},
				{Key: "WORLDLINE_PULL_INTERVAL", Service: "settlement", Configured: "10s",
					Purpose: "How often to list and download. Real Worldline is checked far less often; this is a lab clock."},
			},
		},
		{
			Title: "Banking Circle",
			Note:  "The retry schedule and batching live in config/banking-circle.json, shown in full below. These are the knobs outside that file.",
			Settings: []Setting{
				{Key: "BC_MTLS", Service: "banking-circle", Configured: "require",
					Purpose: "off | optional | require. Bearer auth applies in every mode: this never makes the API unauthenticated."},
				{Key: "BC_TIME_SCALE", Service: "banking-circle", Configured: "86400",
					Purpose: "Divides every delay in the retry schedule. At 86400 the eleven-step story that really ends 48 hours after the first failure plays out in about four seconds. Set it to 1 for real timing."},
				{Key: "BC_SEED_BATCH_SIZE", Service: "banking-circle", Configured: "5",
					Purpose: "maxNotificationsPerMessage on the seeded convenience subscription. The documented minimum, so batching is visible rather than one-notification messages."},
				{Key: "BC_NOTIFICATION_KEY", Service: "banking-circle", Configured: "(a fake 32-char key)",
					Purpose: "The raw UTF-8 bytes of this 32-character key are the AES-256 key. Not base64-decoded -- a real integration gets this wrong once."},
			},
		},
		{
			Title: "B4B Payments",
			Settings: []Setting{
				{Key: "B4B_JWT_KEY_ID", Service: "b4b, settlement", Configured: "b4b-mock-1",
					Purpose: "The kid header on the RS512 bearer token."},
				{Key: "B4B_PROCESSING_DELAY", Service: "b4b", Configured: "300ms",
					Purpose: "How long a payment sits in each regulatory phase before the next callback."},
			},
		},
	}
	// The lab API's own environment is the one block that can honestly be
	// shown live, so it is, and it is the last one -- an operator looking
	// for a vendor knob should not have to scroll past it.
	own := SettingGroup{
		Title: "The lab API",
		Note:  "Read live from this process. Everything above is what compose.yml ships, not a reading of the running container.",
	}
	for _, kv := range [][2]string{
		{"LISTEN", "Listen address."},
		{"LAB_API_TOKEN", "When set, every call but GET /health must carry Authorization: Bearer <it>. The console adds it for the page."},
		{"CONSOLE_STATE_FILE", "The merchant registry. A plain JSON file you can read, diff and delete."},
		{"CONSOLE_PROBE_INTERVAL", "How often to health-check every service."},
		{"CONSOLE_BROWSE_HOST", "The host the published ports are on, for the \"open in a browser\" links -- inside compose the lab API reaches peers by service name, which means nothing in your browser."},
		{"CONSOLE_CONNECT_HOST", "The address a platform reaches this lab at, for the Connect your platform downloads. Set on a deployed lab, so the .env is right even through an SSH or SSM tunnel; unset, the downloads follow the host the request came in on."},
		{"CONSOLE_BC_DELIVERY_CONFIG", "The delivery table to fall back to when banking-circle cannot be asked what it is running."},
		{"CONSOLE_URL_WORLDLINE", "Where to reach the acquirer. Every service has one of these; the lab API steers each on its port plus 1000, the control port, with no vendor login (CONSOLE_CONTROL_URL_<ID> overrides it). \"off\" means this deployment does not run that service, and it is left out."},
		{"CONSOLE_URL_CLOCK", "Where to reach the lab clock. \"off\" on a lab deployed without one (the cloud profile), which is then on the real time."},
		{"CONSOLE_URL_B4B", "Where to reach the payout rail."},
		{"CONSOLE_URL_BANKING_CIRCLE", "Where to reach Banking Circle's credentialed listener."},
		{"CONSOLE_URL_BANK", "Where to reach the core ledger."},
	} {
		own.Settings = append(own.Settings, Setting{
			Key: kv[0], Service: "lab-api", Purpose: kv[1], Live: redact(kv[0], os.Getenv(kv[0])),
		})
	}
	return append(groups, own)
}

// redact hides anything that looks like a credential. Everything in this
// lab is fake, but a console that prints secrets teaches the habit of
// putting real ones where it can find them.
func redact(key, value string) string {
	if value == "" {
		return ""
	}
	upper := strings.ToUpper(key)
	if strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") ||
		(strings.Contains(upper, "KEY") && !strings.Contains(upper, "KEY_ID") && !strings.Contains(upper, "KEY_PATH")) {
		return "(set, hidden)"
	}
	return value
}
