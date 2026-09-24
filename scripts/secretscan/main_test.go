package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeBotToken is shaped like a Telegram bot token, built up so that
// GitHub's own secret scanning does not flag this file.
var fakeBotToken = "123456789:" + "AA" + "HdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

// textbookWIF is the Bitcoin wiki's example key, public for years. It's
// split so key scanners, including Antelope ones that read the same format,
// don't report this file.
var textbookWIF = "5HueCGU8rMjxEXxiPuD5" + "BDku4MkFqeZyd4dZ1jvhTVqvbTLvyTJ"

func TestScanLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		// Secrets. These are generated for this test and hold nothing.
		{`  "privateKeys": ["` + "5a" + `"]`, true},
		{`PrivateKey-ewoqjP7PxY4yr3iLTpLisriqt94hdyDFNgchSxGGztUrTXtNN`, true},
		{`token=` + fakeBotToken, true},
		{`private key: 0c28fca386c7a227600b2fe50b7cae11ec86d3bf1fbe471be89827e19d72aa1d`, true},
		{textbookWIF, true}, // a textbook WIF
		{`password: "hunter2hunter2hunter2hunter2"`, true},
		{`-----BEGIN EC PRIVATE KEY-----`, true},
		// A Sparkle key as generate_keys -x exports it: 32 bytes, base64. Built
		// here so this file doesn't hold one.
		{strings.Repeat("Ab3+", 10) + "Ab3=", true},
		{`<key>SUPublicEDKey</key><string>` + strings.Repeat("Ab3+", 10) + `Ab3=</string>`, false},

		// Not secrets.
		{`peg address AAvNfukpAa4iTcRJPetxuxX8XxbC5gFqUM`, false},
		{`chain 2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy`, false},
		{`txid 74e053f6c1f0d7a2e0e5b6b3b4d1a0f9c8e7d6c5b4a39281706f5e4d3c2b1a0f`, false},
		{`"privateKeys": [],`, false},
		{`token := strings.TrimSpace(string(raw))`, false},
		{textbookWIF[:len(textbookWIF)-1] + "j", false}, // bad checksum
		{`// Private key: 0c28fca386c7a227600b2fe50b7cae11ec86d3bf1fbe471be89827e19d72aa1d secretscan:allow`, false},
	} {
		got := len(scanLine(tc.line)) > 0
		require.Equal(t, tc.want, got, tc.line)
	}
}

func TestScanDiffFiles(t *testing.T) {
	diff := []byte(`diff --git a/deploy/signers.json b/deploy/signers.json
new file mode 100644
--- /dev/null
+++ b/deploy/signers.json
@@ -0,0 +1 @@
+{"required": 2}
diff --git a/cmd/dogevm/signers.go b/cmd/dogevm/signers.go
--- a/cmd/dogevm/signers.go
+++ b/cmd/dogevm/signers.go
@@ -10,0 +11 @@
+// signers hold keys
diff --git a/btcd/btcutil/wif_test.go b/btcd/btcutil/wif_test.go
--- a/btcd/btcutil/wif_test.go
+++ b/btcd/btcutil/wif_test.go
@@ -1,0 +2 @@
+	"` + textbookWIF + `",
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -40,0 +41 @@
+bot token ` + fakeBotToken + `
`)
	found := scanDiff("", diff)
	require.Len(t, found, 2, "%v", found)
	require.Equal(t, "deploy/signers.json", found[0].where)
	require.Equal(t, "README.md:41", found[1].where)
}

func TestForbiddenFiles(t *testing.T) {
	for name, want := range map[string]bool{
		"signers.json":                     true,
		"deploy/mainnet/signers-2.json":    true,
		"var/cosigners.json":               true,
		"node/staking/staker.key":          true,
		"bridge.env":                       true,
		".env":                             true,
		"secrets/telegram-token":           true,
		"secrets.json":                     true,
		"p-chain-key.json":                 true,
		"update-key.txt":                   true,
		"sparkle-private-key.txt":          true,
		"cmd/dogevm/signers.go":            false,
		"scripts/secretscan/main.go":       false,
		".github/workflows/secretscan.yml": false,
		".secretscan-ignore":               false,
		"config/testnet.json":              false,
	} {
		got := forbiddenFile.MatchString(name) && !allowedFile.MatchString(name)
		require.Equal(t, want, got, name)
	}
}
