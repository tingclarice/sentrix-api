// Command vapidgen creates a VAPID key pair for Web Push.
//
//	go run ./cmd/vapidgen            print both keys (redirect it yourself if you want a file)
//	go run ./cmd/vapidgen -env .env  append both keys to .env and print only the public key,
//	                                 so the private key never appears in a terminal or log
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/SherClockHolmes/webpush-go"
)

func main() {
	envFile := flag.String("env", "", "append VAPID_PUBLIC_KEY and VAPID_PRIVATE_KEY to this file")
	flag.Parse()

	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		log.Fatal(err)
	}

	if *envFile == "" {
		fmt.Printf("VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n", public, private)
		return
	}

	if existing, err := os.ReadFile(*envFile); err == nil && strings.Contains(string(existing), "VAPID_PRIVATE_KEY=") {
		log.Fatalf("%s already has VAPID_PRIVATE_KEY; refusing to overwrite it (changing keys invalidates every browser subscription)", *envFile)
	}
	f, err := os.OpenFile(*envFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\nVAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n", public, private); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Keys appended to %s. Public key (for the frontend's NEXT_PUBLIC_VAPID_PUBLIC_KEY):\n%s\n", *envFile, public)
}
