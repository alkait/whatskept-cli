package backup

import "testing"

func TestNormalizeNumber(t *testing.T) {
	cases := map[string]string{
		"+971 50 000 0000":            "+971500000000",
		"971500000000@s.whatsapp.net": "+971500000000",
		"971500000000":                "+971500000000",
		"+1 (555) 123-4567":           "+15551234567",
		"no digits here":              "",
		"":                            "",
		// WhatsApp's obfuscated OwnPhoneNumber must never pass.
		"/ikQLT8+qoSrGb5W5Xag7A==": "",
		"8557":                     "", // too short
		"user@example.com":         "", // wrong suffix
		"12345678901234567890":     "", // too long
	}
	for in, want := range cases {
		if got := normalizeNumber(in); got != want {
			t.Errorf("normalizeNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func plistWith(kv string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>` + kv + `</dict></plist>`)
}

func TestNumberFromPrefsKnownKey(t *testing.T) {
	data := plistWith(`<key>OwnJabberID</key><string>15551234567@s.whatsapp.net</string>`)
	if got := numberFromPrefs(data); got != "+15551234567" {
		t.Errorf("got %q", got)
	}
}

// The real-world case: OwnPhoneNumber is an obfuscated blob, OwnJabberID
// holds the truth. JID keys must win.
func TestNumberFromPrefsPrefersJIDOverObfuscated(t *testing.T) {
	data := plistWith(`
		<key>OwnPhoneNumber</key><string>/ikQLT8+qoSrGb5W5Xag7A==</string>
		<key>OwnJabberID</key><string>971500000000@s.whatsapp.net</string>`)
	if got := numberFromPrefs(data); got != "+971500000000" {
		t.Errorf("got %q, want +971500000000", got)
	}
}

func TestNumberFromPrefsJIDFallback(t *testing.T) {
	data := plistWith(`<key>SomeUnknownKey</key><string>15551234567@s.whatsapp.net</string>`)
	if got := numberFromPrefs(data); got != "+15551234567" {
		t.Errorf("got %q", got)
	}
}

func TestNumberFromPrefsNothingUseful(t *testing.T) {
	if got := numberFromPrefs(plistWith(`<key>FullUserName</key><string>Ayman</string>`)); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := numberFromPrefs([]byte("not a plist")); got != "" {
		t.Errorf("garbage: got %q, want empty", got)
	}
}
