// Command secretscan stops private keys, tokens and passwords reaching the
// repository. The git hooks in .githooks run it before every commit and
// push, and CI runs it on every push as a backstop.
//
//	secretscan staged        lines and files staged for commit
//	secretscan push          commits being pushed (git pre-push hook input on stdin)
//	secretscan range A..B    commits in a range
//	secretscan history       every commit on every branch
//
// It exits 1 if it finds anything. A line that is a deliberate, public test
// vector can carry the marker "secretscan:allow".
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

// finding is one suspected secret.
type finding struct {
	where, rule string
}

// forbiddenFile matches the names of files that hold secrets in a
// deployment: signer sets (which may carry private keys), key files, tokens,
// passwords and bridge state.
var forbiddenFile = regexp.MustCompile(`(?i)(^|/)(` +
	`(co)?signers[^/]*\.json|signing-log[^/]*\.json|faucet\.json|deposits\.json|p-chain-key\.json|` +
	`[^/]*\.key|[^/]*\.pem|[^/]*\.p12|\.env(\.[^/]*)?|[^/]*\.env|` +
	`telegram-token|rpc-password|secrets?(\.[^/]*)?|[^/]*\.wif|id_(rsa|ed25519|ecdsa)|` +
	// Sparkle's update-signing key, as generate_keys -x exports it.
	`update-key[^/]*|[^/]*sparkle[^/]*key[^/]*|[^/]*private[-_]?key[^/]*` +
	`)$`)

// allowedFile matches committed files whose names look like secrets but are
// not: source code about keys, and upstream btcd test data.
var allowedFile = regexp.MustCompile(`\.go$|\.md$|^btcd/.*(testdata|_test\.go|/data/)`)

// contentRules match secrets in a line of text.
var contentRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"signer set with private keys", regexp.MustCompile(`"privateKeys"\s*:\s*\[\s*"`)},
	{"Metal/Avalanche private key", regexp.MustCompile(`PrivateKey-[1-9A-HJ-NP-Za-km-z]{40,}`)},
	{"PEM private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"Telegram bot token", regexp.MustCompile(`\b\d{8,10}:AA[A-Za-z0-9_-]{30,}`)},
	{"hex private key", regexp.MustCompile(`(?i)(priv|secret|seed|wif|mnemonic)[^\n]{0,40}\b(0x)?[0-9a-f]{64}\b`)},
	{"credential assignment", regexp.MustCompile(`(?i)\b(api[_-]?key|auth[_-]?token|bot[_-]?token|password|passwd|secret)\b["']?\s*[:=]\s*["']?[A-Za-z0-9+/_\-]{20,}`)},
	{"GitHub token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|github_pat)_[A-Za-z0-9_]{30,}`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	// An exported Sparkle (EdDSA) private key is a bare base64 line: the
	// 32-byte seed (44 characters) or seed and public key (88). The public key
	// in Info.plist sits inside <string> tags, so it does not match.
	{"Sparkle update-signing key", regexp.MustCompile(`^\s*([A-Za-z0-9+/]{43}=|[A-Za-z0-9+/]{86}==)\s*$`)},
}

// wifCandidate matches strings shaped like a WIF private key; isWIF then
// checks the base58 checksum, so addresses and hashes do not match.
var wifCandidate = regexp.MustCompile(`\b[1-9A-HJ-NP-Za-km-z]{51,52}\b`)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func isWIF(s string) bool {
	n := new(big.Int)
	for _, c := range s {
		i := strings.IndexRune(base58Alphabet, c)
		if i < 0 {
			return false
		}
		n.Mul(n, big.NewInt(58)).Add(n, big.NewInt(int64(i)))
	}
	raw := n.Bytes()
	for _, c := range s {
		if c != '1' {
			break
		}
		raw = append([]byte{0}, raw...)
	}
	// version || 32-byte key || optional 0x01 || 4-byte checksum
	if len(raw) != 37 && len(raw) != 38 {
		return false
	}
	if len(raw) == 38 && raw[33] != 1 {
		return false
	}
	body, sum := raw[:len(raw)-4], raw[len(raw)-4:]
	first := sha256.Sum256(body)
	second := sha256.Sum256(first[:])
	return bytes.Equal(second[:4], sum)
}

// scanLine returns the rules line breaks.
func scanLine(line string) []string {
	if strings.Contains(line, "secretscan:allow") {
		return nil
	}
	var hits []string
	for _, r := range contentRules {
		if r.re.MatchString(line) {
			hits = append(hits, r.name)
		}
	}
	for _, m := range wifCandidate.FindAllString(line, -1) {
		if isWIF(m) {
			hits = append(hits, "WIF private key")
			break
		}
	}
	return hits
}

// upstreamVector reports whether file is upstream btcd test data, whose
// well-known keys are public, or this scanner's own test data.
func upstreamVector(file string) bool {
	switch file {
	case "scripts/secretscan/main_test.go", // the scanner's own fake secrets
		"cmd/dogevm/testdata/wallet-vectors.json": // keys are sha256 of public labels
		return true
	}
	return strings.HasPrefix(file, "btcd/") &&
		(strings.HasSuffix(file, "_test.go") || strings.Contains(file, "/testdata/") ||
			strings.Contains(file, "/data/") || path.Base(file) == "README.md" ||
			strings.HasSuffix(file, "/doc.go"))
}

// scanDiff scans the added lines and new files of a unified diff (as
// produced by git with --unified=0).
func scanDiff(label string, diff []byte) []finding {
	var found []finding
	var file string
	lineNo := 0
	sc := bufio.NewScanner(bytes.NewReader(diff))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if file != "/dev/null" && forbiddenFile.MatchString(file) && !allowedFile.MatchString(file) {
				found = append(found, finding{label + file, "file that holds secrets in a deployment"})
			}
		case strings.HasPrefix(line, "@@ "):
			// @@ -a,b +c,d @@
			var c int
			if i := strings.Index(line, "+"); i >= 0 {
				fmt.Sscanf(line[i+1:], "%d", &c)
			}
			lineNo = c - 1
		case strings.HasPrefix(line, "+"):
			lineNo++
			if upstreamVector(file) {
				continue
			}
			for _, rule := range scanLine(line[1:]) {
				found = append(found, finding{fmt.Sprintf("%s%s:%d", label, file, lineNo), rule})
			}
		}
	}
	return found
}

// withoutIgnored drops findings listed in .secretscan-ignore at the top of
// the repository: past commits, reviewed and known to hold nothing of ours.
func withoutIgnored(found []finding) []finding {
	raw, err := os.ReadFile(strings.TrimSpace(string(git("rev-parse", "--show-toplevel"))) + "/.secretscan-ignore")
	if err != nil {
		return found
	}
	ignored := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			ignored[line] = true
		}
	}
	var kept []finding
	for _, f := range found {
		if !ignored[f.where] {
			kept = append(kept, f)
		}
	}
	return kept
}

func git(args ...string) []byte {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "secretscan: git %s: %v\n", strings.Join(args, " "), err)
		os.Exit(2)
	}
	return out
}

// scanCommits scans each commit separately, so a secret added and removed
// again within the range is still caught: it would stay in history.
func scanCommits(revs ...string) []finding {
	var found []finding
	list := strings.Fields(string(git(append([]string{"rev-list"}, revs...)...)))
	for _, c := range list {
		diff := git("show", "--format=", "--unified=0", "--no-color", "--no-renames", "--first-parent", "-m", c)
		found = append(found, scanDiff(c[:10]+" ", diff)...)
	}
	return found
}

// pushedCommits reads the pre-push hook's stdin, one line per ref:
// <local ref> <local sha> <remote ref> <remote sha>.
func pushedCommits() []finding {
	var found []finding
	zero := strings.Repeat("0", 40)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 || f[1] == zero {
			continue // malformed, or a branch deletion
		}
		if f[3] == zero {
			// A new branch: everything not already on the remote.
			found = append(found, scanCommits(f[1], "--not", "--remotes")...)
		} else {
			found = append(found, scanCommits(f[3]+".."+f[1])...)
		}
	}
	return found
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: secretscan staged | push | range A..B | history")
		os.Exit(2)
	}
	var found []finding
	switch os.Args[1] {
	case "staged":
		found = scanDiff("", git("diff", "--cached", "--unified=0", "--no-color", "--no-renames"))
	case "push":
		found = pushedCommits()
	case "range":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: secretscan range A..B")
			os.Exit(2)
		}
		found = scanCommits(os.Args[2])
	case "history":
		found = scanCommits("--all")
	default:
		fmt.Fprintf(os.Stderr, "secretscan: unknown mode %q\n", os.Args[1])
		os.Exit(2)
	}
	found = withoutIgnored(found)
	if len(found) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "secretscan: possible secrets; nothing was committed or pushed:")
	for _, f := range found {
		fmt.Fprintf(os.Stderr, "  %s: %s\n", f.where, f.rule)
	}
	fmt.Fprintln(os.Stderr, `Remove them, or mark a public test vector with "secretscan:allow".`)
	os.Exit(1)
}
