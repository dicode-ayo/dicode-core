package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/webhooksign"
)

// cmdWebhook implements `dicode webhook <subcommand>` — local, daemon-free
// helpers for working with dicode's webhook HMAC scheme. Mirrors cmdDeno/
// cmdPython/cmdRelock: pure local logic, no daemon connection needed.
func cmdWebhook(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dicode webhook sign [flags]")
	}
	switch args[0] {
	case "sign":
		return cmdWebhookSign(args[1:])
	default:
		return fmt.Errorf("unknown webhook subcommand %q — supported: sign", args[0])
	}
}

const webhookSignUsage = `Usage: dicode webhook sign --secret <secret> [flags]

Signs a webhook body with dicode's HMAC-SHA256 scheme and prints the header
lines a caller needs to send a protected webhook request (see
pkg/trigger/webhook.go and docs/webhooks.md).

Flags:
  --secret <value>       HMAC secret (required)
  --data <string>        inline request body
  --data-file <path>     read the request body from a file
                          (default: read the body from stdin if neither
                          --data nor --data-file is given)
  --timestamp <unix-sec> explicit Unix timestamp to sign
                          (default: current time; mutually exclusive with
                          --no-timestamp)
  --no-timestamp         omit the timestamp and sign the bare body
                          (GitHub-compatible mode; mutually exclusive with
                          --timestamp)

Output (stdout):
  X-Hub-Signature-256: sha256=<hex>
  X-Dicode-Timestamp: <unix_ts>

The second line is omitted when --no-timestamp is given.`

// cmdWebhookSign implements `dicode webhook sign`. It computes the same
// hex(HMAC-SHA256(secret, preimage)) digest pkg/trigger/webhook.go verifies
// (via pkg/webhooksign, the shared source of truth) and prints the header
// lines a caller needs to send a signed request to a protected dicode
// webhook — no running daemon required.
func cmdWebhookSign(args []string) error {
	var secret, data, dataFile, timestamp string
	var haveData, haveDataFile, haveTimestamp, noTimestamp bool

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--help", a == "-h":
			fmt.Fprintln(os.Stderr, webhookSignUsage)
			return nil
		case a == "--secret":
			v, n, err := webhookFlagValue(args, i, "--secret")
			if err != nil {
				return err
			}
			secret, i = v, n
		case strings.HasPrefix(a, "--secret="):
			secret = strings.TrimPrefix(a, "--secret=")
		case a == "--data":
			v, n, err := webhookFlagValue(args, i, "--data")
			if err != nil {
				return err
			}
			data, i, haveData = v, n, true
		case strings.HasPrefix(a, "--data="):
			data, haveData = strings.TrimPrefix(a, "--data="), true
		case a == "--data-file":
			v, n, err := webhookFlagValue(args, i, "--data-file")
			if err != nil {
				return err
			}
			dataFile, i, haveDataFile = v, n, true
		case strings.HasPrefix(a, "--data-file="):
			dataFile, haveDataFile = strings.TrimPrefix(a, "--data-file="), true
		case a == "--timestamp":
			v, n, err := webhookFlagValue(args, i, "--timestamp")
			if err != nil {
				return err
			}
			timestamp, i, haveTimestamp = v, n, true
		case strings.HasPrefix(a, "--timestamp="):
			timestamp, haveTimestamp = strings.TrimPrefix(a, "--timestamp="), true
		case a == "--no-timestamp":
			noTimestamp = true
		default:
			return fmt.Errorf("unknown flag %q\n\n%s", a, webhookSignUsage)
		}
	}

	if secret == "" {
		return fmt.Errorf("--secret is required\n\n%s", webhookSignUsage)
	}
	if haveData && haveDataFile {
		return fmt.Errorf("--data and --data-file are mutually exclusive")
	}
	if haveTimestamp && noTimestamp {
		return fmt.Errorf("--timestamp and --no-timestamp are mutually exclusive")
	}

	var body []byte
	switch {
	case haveData:
		body = []byte(data)
	case haveDataFile:
		b, err := os.ReadFile(dataFile) // #nosec G304 — operator-supplied path via explicit CLI flag.
		if err != nil {
			return fmt.Errorf("read --data-file %q: %w", dataFile, err)
		}
		body = b
	default:
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read body from stdin: %w", err)
		}
		body = b
	}

	// tsStr is the exact string folded into the HMAC preimage and emitted in
	// the X-Dicode-Timestamp header — empty means "sign the bare body"
	// (GitHub-compatible mode), matching webhooksign.PreimageDigest's
	// contract.
	var tsStr string
	switch {
	case noTimestamp:
		tsStr = ""
	case haveTimestamp:
		if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
			return fmt.Errorf("invalid --timestamp %q: must be a Unix timestamp in seconds", timestamp)
		}
		tsStr = timestamp
	default:
		// Bound at call time — this is a leaf CLI command, no clock
		// injection needed.
		tsStr = strconv.FormatInt(time.Now().Unix(), 10)
	}

	digest := webhooksign.PreimageDigest(secret, tsStr, body)
	fmt.Printf("%s: %s\n", webhooksign.SignatureHeader, webhooksign.SignatureValue(digest))
	if tsStr != "" {
		fmt.Printf("%s: %s\n", webhooksign.TimestampHeader, tsStr)
	}
	return nil
}

// webhookFlagValue reads the value for a "--flag value" pair at args[i],
// returning the value and the new index (i+1, so the caller's loop skips
// past the consumed value). Shared by cmdWebhookSign's flag cases.
func webhookFlagValue(args []string, i int, name string) (string, int, error) {
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("%s requires a value", name)
	}
	return args[i+1], i + 1, nil
}
