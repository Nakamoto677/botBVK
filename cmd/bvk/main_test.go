package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"bot-bvk/internal/telegram"
)

func TestCheckCannotReportSuccessWhenTelegramCheckIsSkipped(t *testing.T) {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	flag.CommandLine = flag.NewFlagSet("bvk-test", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = []string{"bvk-test", "-check", "-skip-telegram-check"}
	err := run()
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("verification must fail before connecting to a database: %v", err)
	}
}

func TestDisableBrokenClientGroupsKeepsTransientChats(t *testing.T) {
	groups := map[int64]telegram.ClientGroup{-1001: {CompanyID: 1}, -1002: {CompanyID: 2}}
	problems := []telegram.ClientGroupProblem{
		{ChatID: -1001, Err: errors.New("telegram 400: Bad Request: chat not found"), Hint: "add the bot", Transient: false},
		{ChatID: -1002, Err: errors.New("telegram 500: Internal Server Error"), Transient: true},
	}
	if disabled := disableBrokenClientGroups(groups, problems); disabled != 1 {
		t.Fatalf("disabled: %d", disabled)
	}
	if _, ok := groups[-1001]; ok {
		t.Fatal("a wrong chat_id must not stay admitted")
	}
	if _, ok := groups[-1002]; !ok {
		t.Fatal("a transient failure must not drop a healthy chat")
	}
}
