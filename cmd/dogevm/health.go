package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// check is one health check's result.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// healthChecker runs the checks behind /api/health and dogevm monitor.
type healthChecker struct {
	b *bridge

	// P-Chain API and the L1 validator to watch; empty skips the check.
	pChainURI    string
	validationID string
	minBalance   float64 // METAL

	// A deposit or peg-out this many blocks past due means the bridge has
	// stopped.
	stallBlocks int64
}

const nMETALPerMETAL = 1_000_000_000

func (h *healthChecker) register(fs *flag.FlagSet) {
	fs.StringVar(&h.pChainURI, "pchain-uri", "http://127.0.0.1:9660/ext/bc/P", "P-Chain API for the validator balance check")
	fs.StringVar(&h.validationID, "validation-id", "", "the L1 validator's validation ID (empty skips the balance check)")
	fs.Float64Var(&h.minBalance, "min-validator-balance", 1, "alert when the validator's balance falls below this many METAL")
	fs.Int64Var(&h.stallBlocks, "stall-blocks", 10, "alert when a deposit or peg-out is this many blocks overdue")
}

// run performs every check. state may be nil if it could not be loaded.
func (h *healthChecker) run(state *pegState, loadErr error) []check {
	var checks []check

	checks = append(checks, h.pauseCheck())

	// The peg.
	switch {
	case loadErr != nil:
		checks = append(checks, check{"peg", false, "cannot read both chains: " + loadErr.Error()})
	default:
		a := h.b.audit(state)
		detail := fmt.Sprintf("locked %s, circulating %s, pending in %s, pending out %s",
			formatDoge(a.Locked), formatDoge(a.Circulating), formatDoge(a.PendingPegIns), formatDoge(a.PendingPegOuts))
		checks = append(checks, check{"peg", a.solvent(), detail})
		checks = append(checks, h.bridgeLiveness(state))
	}

	checks = append(checks, h.dogecoinNode())
	checks = append(checks, h.dogecoinVM())
	if h.validationID != "" {
		checks = append(checks, h.validatorBalance())
	}
	return checks
}

// pauseCheck fails while the bridge is paused, so the pause is alerted on.
func (h *healthChecker) pauseCheck() check {
	if p := h.b.paused(); p != nil {
		return check{"pause", false, fmt.Sprintf("paused since %s: %s", p.Since.UTC().Format(time.RFC3339), p.Reason)}
	}
	return check{"pause", true, "not paused"}
}

// bridgeLiveness fails if a deposit or peg-out is well past due.
func (h *healthChecker) bridgeLiveness(s *pegState) check {
	for _, d := range s.deposits {
		if _, done := s.released[d.outPoint]; done {
			continue
		}
		wouldCredit := h.b.maxCirculating == 0 || s.reserveCreated-s.reserveUnspent+d.value <= h.b.maxCirculating
		if wouldCredit && d.confirmations > h.b.confirmationsFor(d.value)+h.stallBlocks {
			return check{"bridge", false, fmt.Sprintf("deposit %v has %d confirmations and is not credited", d.outPoint, d.confirmations)}
		}
	}
	for _, p := range s.pegOuts {
		if _, done := s.paid[p.txid]; !done && p.confirmations > h.stallBlocks {
			return check{"bridge", false, fmt.Sprintf("peg-out %v is %d blocks old and not paid", p.txid, p.confirmations)}
		}
	}
	return check{"bridge", true, "nothing overdue"}
}

func (h *healthChecker) dogecoinNode() check {
	rpc := h.b.doge.(*dogeChain).rpc
	var info struct {
		Blocks               int64   `json:"blocks"`
		Headers              int64   `json:"headers"`
		BestBlockHash        string  `json:"bestblockhash"`
		VerificationProgress float64 `json:"verificationprogress"`
	}
	if err := rpc.call(&info, "getblockchaininfo"); err != nil {
		return check{"dogecoin", false, "Dogecoin Core is not answering: " + err.Error()}
	}
	if info.Blocks < info.Headers-6 {
		// Progress is by transactions: recent blocks are full, so it is a
		// better guide than the block count.
		return check{"dogecoin", false, fmt.Sprintf("still syncing, %.0f%% done (block %d of %d); deposits are credited once it catches up",
			info.VerificationProgress*100, info.Blocks, info.Headers)}
	}
	var header struct {
		Time int64 `json:"time"`
	}
	if err := rpc.call(&header, "getblockheader", info.BestBlockHash); err != nil {
		return check{"dogecoin", false, err.Error()}
	}
	// Dogecoin targets a block a minute; half an hour without one means
	// the node has fallen behind.
	age := time.Since(time.Unix(header.Time, 0)).Round(time.Second)
	if age > 30*time.Minute {
		return check{"dogecoin", false, fmt.Sprintf("latest block %d is %s old", info.Blocks, age)}
	}
	return check{"dogecoin", true, fmt.Sprintf("block %d, %s old", info.Blocks, age)}
}

func (h *healthChecker) dogecoinVM() check {
	var height int64
	if err := h.b.vm.(*vmChain).rpc.call(&height, "getblockcount"); err != nil {
		return check{"dogecoinvm", false, "DogecoinVM is not answering: " + err.Error()}
	}
	return check{"dogecoinvm", true, fmt.Sprintf("block %d", height)}
}

// validatorBalance checks the L1 validator can keep paying its continuous
// fee; at zero the validator is deactivated and the chain stops.
func (h *healthChecker) validatorBalance() check {
	var res struct {
		Result struct {
			Balance string `json:"balance"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "platform.getL1Validator",
		"params": map[string]string{"validationID": h.validationID},
	})
	resp, err := http.Post(h.pChainURI, "application/json", bytes.NewReader(body))
	if err != nil {
		return check{"validator", false, err.Error()}
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return check{"validator", false, err.Error()}
	}
	if res.Error != nil {
		return check{"validator", false, res.Error.Message}
	}
	balance, err := strconv.ParseInt(res.Result.Balance, 10, 64)
	if err != nil {
		return check{"validator", false, "unreadable balance " + res.Result.Balance}
	}
	// At Metal's minimum validator fee of 512 nMETAL a second.
	days := float64(balance) / 512 / 86400
	detail := fmt.Sprintf("%.3f METAL, about %.0f days at the minimum fee", float64(balance)/nMETALPerMETAL, days)
	return check{"validator", float64(balance) >= h.minBalance*nMETALPerMETAL, detail}
}

func allOK(checks []check) bool {
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// alerter delivers monitor alerts.
type alerter struct {
	webhook       string // Slack or Discord incoming webhook
	telegramToken string // Telegram bot token
	telegramChat  string // Telegram chat ID
}

func (a alerter) send(msg string) {
	if a.webhook != "" {
		if err := postWebhook(a.webhook, msg); err != nil {
			log.Printf("webhook: %v", err)
		}
	}
	if a.telegramToken != "" && a.telegramChat != "" {
		if err := postTelegram(a.telegramToken, a.telegramChat, msg); err != nil {
			log.Printf("telegram: %v", err)
		}
	}
}

// postTelegram sends msg to a Telegram chat through a bot.
func postTelegram(token, chatID, msg string) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id": chatID, "text": msg, "disable_web_page_preview": true,
	})
	resp, err := http.Post("https://api.telegram.org/bot"+token+"/sendMessage", "application/json", bytes.NewReader(body))
	if err != nil {
		// The URL holds the token; do not log it.
		return errors.New("request to Telegram failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var res struct {
			Description string `json:"description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, res.Description)
	}
	return nil
}

// monitor runs the checks every interval and alerts when a check changes
// state, and again every remind while one is failing.
func (h *healthChecker) monitor(interval, remind time.Duration, alert alerter) {
	last := map[string]bool{}
	var lastAlert time.Time
	for {
		state, err := h.b.load()
		checks := h.run(state, err)

		var changed []string
		for _, c := range checks {
			if prev, seen := last[c.Name]; !seen || prev != c.OK {
				status := "OK"
				if !c.OK {
					status = "FAILING"
				}
				if seen || !c.OK {
					changed = append(changed, fmt.Sprintf("%s %s: %s", c.Name, status, c.Detail))
				}
			}
			last[c.Name] = c.OK
		}
		for _, c := range checks {
			log.Printf("%-10s ok=%-5t %s", c.Name, c.OK, c.Detail)
		}

		reminder := !allOK(checks) && time.Since(lastAlert) > remind
		if len(changed) > 0 || reminder {
			msg := "DogecoinVM bridge: "
			if allOK(checks) {
				msg += "all checks OK"
			} else {
				msg += "a check is failing"
			}
			for _, line := range changed {
				msg += "\n- " + line
			}
			if len(changed) == 0 {
				for _, c := range checks {
					if !c.OK {
						msg += "\n- still failing: " + c.Name + ": " + c.Detail
					}
				}
			}
			log.Print(msg)
			alert.send(msg)
			lastAlert = time.Now()
		}
		time.Sleep(interval)
	}
}

// postWebhook sends msg to a Slack ("text") or Discord ("content") incoming
// webhook; each ignores the other's field.
func postWebhook(url, msg string) error {
	body, _ := json.Marshal(map[string]string{"text": msg, "content": msg})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func cmdMonitor(args []string) error {
	var s settings
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file (public keys are enough)")
	depositsPath := fs.String("deposits", "", "deposit address registry (default: deposits.json next to -signers)")
	webhook := fs.String("webhook", "", "Slack or Discord incoming webhook URL for alerts")
	telegramTokenFile := fs.String("telegram-token-file", "", "file holding a Telegram bot token, for alerts")
	telegramChat := fs.String("telegram-chat", "", "Telegram chat ID to send alerts to")
	test := fs.Bool("test-alert", false, "send a test alert and exit")
	interval := fs.Duration("interval", time.Minute, "time between checks")
	remind := fs.Duration("remind", 6*time.Hour, "repeat an alert this often while a check fails")
	once := fs.Bool("once", false, "run the checks once, print them, and exit non-zero if one fails")
	s.register(fs)
	b := bridgeFlags(fs)
	h := &healthChecker{b: b}
	h.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := s.resolve(); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	if err := b.connect(&s, signers); err != nil {
		return err
	}
	b.registry = registryFor(*depositsPath, *signersPath)

	if *once {
		state, err := b.load()
		checks := h.run(state, err)
		printJSON(checks)
		if !allOK(checks) {
			return fmt.Errorf("a check is failing")
		}
		return nil
	}
	alert := alerter{webhook: *webhook, telegramChat: *telegramChat}
	if *telegramTokenFile != "" {
		token, err := os.ReadFile(*telegramTokenFile)
		if err != nil {
			return err
		}
		alert.telegramToken = strings.TrimSpace(string(token))
	}
	if *test {
		alert.send("DogecoinVM bridge: test alert. Alerts are working.")
		return nil
	}
	h.monitor(*interval, *remind, alert)
	return nil
}
