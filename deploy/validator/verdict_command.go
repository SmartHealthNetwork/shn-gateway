package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

const pasVersionEnv = "hapi.fhir.implementationguides.pas.version"

func verdictCommand(args []string, getenv func(string) string) int {
	if len(args) == 0 || (args[0] != "qualify" && args[0] != "verify-verdicts") {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid command")
		return 1
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	baseDefault := getenv("SHN_VALIDATOR_BASE")
	if baseDefault == "" {
		baseDefault = defaultBase
	}
	base := flags.String("base", baseDefault, "validator FHIR base")
	line := flags.String("line", getenv("SHN_IG_LINE"), "IG line")
	version := flags.String("pas-version", getenv(pasVersionEnv), "PAS package version")
	budget := flags.Duration("budget", startupBudget, "total qualification budget")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *budget <= 0 {
		fmt.Fprintln(os.Stderr, "verdict: invalid configuration")
		return 1
	}
	normalized, err := loopbackBase(*base)
	if err != nil || !validPASConfiguration(*line, *version) {
		fmt.Fprintln(os.Stderr, "verdict: invalid configuration")
		return 1
	}
	var rows []warmup
	if command == "qualify" {
		rows = append(rows, qualificationRows(*line, "prime")...)
		rows = append(rows, qualificationRows(*line, "qualify-1")...)
		rows = append(rows, qualificationRows(*line, "qualify-2")...)
	} else {
		rows = append(rows, qualificationRows(*line, "verify")...)
	}
	rows = append(rows, negativeRows(*line)...)
	rows = append(rows, fullResponseRows(*line)...)
	prepared := make([][]byte, len(rows))
	for i, row := range rows {
		prepared[i], err = fixtureBody(row)
		if err != nil {
			fmt.Fprintln(os.Stderr, "verdict: fixture unavailable")
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *budget)
	defer cancel()
	client := httpClient()
	for i, row := range rows {
		started := time.Now()
		if err := submitValidation(ctx, client, normalized, row, prepared[i]); err != nil {
			fmt.Fprintf(os.Stderr, "verdict: line=%s row=%s elapsed=%s outcome=failed reason=%s\n", *line, row.identity, time.Since(started).Round(time.Millisecond), err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "verdict: line=%s row=%s elapsed=%s outcome=completed\n", *line, row.identity, time.Since(started).Round(time.Millisecond))
	}
	return 0
}

func validPASConfiguration(line, version string) bool {
	want, ok := pasVersion(line)
	return ok && version != "" && version == want
}

func loopbackBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("invalid base")
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("base is not loopback")
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}
