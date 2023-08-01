// Copyright (c) 2019-2022, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sylabs/singularity/v4/internal/pkg/util/bin"
	"github.com/sylabs/singularity/v4/pkg/util/cryptkey"
)

const (
	// Passphrase used for passphrase-based encryption tests
	Passphrase = "e2e-passphrase"
)

// CheckCryptsetupVersion checks the version of cryptsetup and returns
// an error if the version is not compatible; nil otherwise
func CheckCryptsetupVersion() error {
	cryptsetup, err := bin.FindBin("cryptsetup")
	if err != nil {
		return err
	}

	cmd := exec.Command(cryptsetup, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run cryptsetup --version: %s", err)
	}

	if !strings.Contains(string(out), "cryptsetup 2.") {
		return fmt.Errorf("incompatible cryptsetup version")
	}

	return nil
}

// GeneratePemFiles creates a new PEM file for testing purposes.
func GeneratePemFiles(t *testing.T, basedir string) (string, string) {
	// Temporary file to save the PEM public file. The caller is in charge of cleanup
	tempPemPubFile, err := os.CreateTemp(basedir, "pem-pub-")
	if err != nil {
		t.Fatalf("failed to create temporary file: %s", err)
	}
	tempPemPubFile.Close()

	// Temporary file to save the PEM file. The caller is in charge of cleanup
	tempPemPrivFile, err := os.CreateTemp(basedir, "pem-priv-")
	if err != nil {
		t.Fatalf("failed to create temporary file: %s", err)
	}
	tempPemPrivFile.Close()

	rsaKey, err := cryptkey.GenerateRSAKey(2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %s", err)
	}

	err = cryptkey.SavePublicPEM(tempPemPubFile.Name(), rsaKey)
	if err != nil {
		t.Fatalf("failed to generate PEM public file: %s", err)
	}

	err = cryptkey.SavePrivatePEM(tempPemPrivFile.Name(), rsaKey)
	if err != nil {
		t.Fatalf("failed to generate PEM private file: %s", err)
	}

	return tempPemPubFile.Name(), tempPemPrivFile.Name()
}
