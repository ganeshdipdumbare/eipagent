package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ganeshdipdumbare/kiln/audit"
)

// auditCmd verifies a hash-chained audit log, optionally against a head
// copied off-host so a host-level attacker cannot roll the log back.
func auditCmd(args []string) error {
	if len(args) == 0 || args[0] != "verify" {
		return fmt.Errorf("usage: kiln audit verify [-key-env NAME | -key-file FILE] [-anchor FILE] LOG")
	}
	fs := flag.NewFlagSet("audit verify", flag.ExitOnError)
	keyEnv := fs.String("key-env", "", "environment variable holding the HMAC key")
	keyFile := fs.String("key-file", "", "file holding the HMAC key")
	anchorFile := fs.String("anchor", "", "off-host head checkpoint (JSON)")
	fs.Parse(args[1:])
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kiln audit verify [-key-env NAME | -key-file FILE] [-anchor FILE] LOG")
	}
	if (*keyEnv == "") != (*keyFile == "") {
		return fmt.Errorf("set exactly one of -key-env or -key-file")
	}
	var key []byte
	if *keyEnv != "" {
		if v := os.Getenv(*keyEnv); v != "" {
			key = []byte(v)
		}
	}
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		key = []byte(trimNewline(string(b)))
	}
	if len(key) == 0 && (*keyEnv != "" || *keyFile != "") {
		return fmt.Errorf("audit: empty key")
	}
	path := fs.Arg(0)
	if *anchorFile != "" {
		b, err := os.ReadFile(*anchorFile)
		if err != nil {
			return err
		}
		h, err := audit.DecodeHead(b)
		if err != nil {
			return err
		}
		n, err := audit.VerifyKeyedWithAnchor(path, key, h)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %d records verified against the external anchor\n", path, n)
		return nil
	}
	var n int
	var err error
	if len(key) > 0 {
		n, err = audit.VerifyKeyed(path, key)
	} else {
		n, err = audit.Verify(path)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d records verified\n", path, n)
	return nil
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
