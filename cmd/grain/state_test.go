package main

// `grain state status` is what an operator runs to find out where this
// installation's state lives and what survives a rebuilt host -- which
// is exactly why the secrets key is reported there. Everything else in
// that output can be cloned back from the remote; the key cannot, and a
// deployment only finds that out at the moment it no longer has it.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/secrets"
)

func TestStateStatusReportsTheSecretsKeyAndSaysToBackItUp(t *testing.T) {
	dataDir := t.TempDir()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.WriteKeyFile(secretsConfig(dataDir).KeyFile, key); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := stateStatus(context.Background(), dataDir, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{
		secretsConfig(dataDir).KeyFile,
		key.Public(),
		"back this file up",
		// The way to put it back, named where the problem is: a redeploy
		// that has to be told about this file at the moment it is gone is
		// too late.
		"GRAIN_SECRETS_KEY",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("`grain state status` never mentions %q:\n%s", want, out.String())
		}
	}
}

// A command that reports on a key must not be the thing that creates
// one: secrets.Open mints on a data directory that has none, and a
// status run that quietly minted would hand an operator a key their
// repository's secrets file is not encrypted to.
func TestStateStatusDoesNotMintAKeyToReportOne(t *testing.T) {
	dataDir := t.TempDir()

	var out bytes.Buffer
	if err := stateStatus(context.Background(), dataDir, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := os.Stat(secretsConfig(dataDir).KeyFile); !os.IsNotExist(err) {
		t.Fatalf("`grain state status` created %s (%v)", secretsConfig(dataDir).KeyFile, err)
	}
	if !strings.Contains(out.String(), "none yet") {
		t.Errorf("a deployment with no key yet is not reported as such:\n%s", out.String())
	}
}
