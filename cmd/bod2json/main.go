// Command bod2json converts an Infor SyncSupplierInvoice BOD to JSON and can
// post it to an HTTP API.
//
//	bod2json invoice.xml           print the invoice as JSON
//	bod2json < invoice.xml         same, reading standard input
//	bod2json -post invoice.xml     post it; needs BOD_API_URL, BOD_CLIENT_ID
//	                               and BOD_CLIENT_SECRET in the environment
//
// Exit status: 0 success, 1 the BOD is unreadable or invalid, 2 delivery
// failed, 64 wrong usage.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	bod "github.com/kirlandezzz/bod-to-json"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("bod2json", flag.ContinueOnError)
	fs.SetOutput(stderr)
	post := fs.Bool("post", false, "post the invoice to BOD_API_URL instead of printing it")
	timeout := fs.Duration("timeout", 30*time.Second, "total time allowed for posting, retries included")
	attempts := fs.Int("attempts", 4, "maximum number of posting attempts")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: bod2json [-post] [-timeout 30s] [-attempts 4] [file]")
		return 64
	}

	in := stdin
	if fs.NArg() == 1 {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer f.Close()
		in = f
	}

	inv, err := bod.ParseInvoice(in)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := inv.Validate(); err != nil {
		fmt.Fprintf(stderr, "invalid invoice:\n%v\n", err)
		return 1
	}

	if !*post {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(inv); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		return 0
	}

	client := &bod.Client{
		BaseURL:      getenv("BOD_API_URL"),
		TokenURL:     getenv("BOD_TOKEN_URL"),
		ClientID:     getenv("BOD_CLIENT_ID"),
		ClientSecret: getenv("BOD_CLIENT_SECRET"),
		HTTP:         &http.Client{Timeout: 10 * time.Second},
		MaxAttempts:  *attempts,
		BaseDelay:    500 * time.Millisecond,
		MaxDelay:     10 * time.Second,
	}
	if client.BaseURL == "" || client.ClientID == "" || client.ClientSecret == "" {
		fmt.Fprintln(stderr, "-post needs BOD_API_URL, BOD_CLIENT_ID and BOD_CLIENT_SECRET")
		return 64
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := client.PostInvoice(ctx, inv); err != nil {
		fmt.Fprintln(stderr, "post failed:", err)
		return 2
	}
	fmt.Fprintf(stdout, "posted invoice %s (idempotency key %s)\n", inv.ID, inv.IdempotencyKey())
	return 0
}
