package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("grabr: %v", err)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "serve":
		return runServe(args)
	case "crawl":
		return runCrawl(args)
	case "migrate":
		return runMigrate(args)
	default:
		return fmt.Errorf("unknown command %q (valid: serve, crawl, migrate)", cmd)
	}
}
