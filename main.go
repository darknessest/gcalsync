package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: gcalsync (add|sync|desync|cleanup|list|reconcile)")
		os.Exit(1)
	}
	config, err := readConfig(".gcalsync.toml")
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}
	initOAuthConfig(config)
	dbInit()
	command := os.Args[1]
	switch command {
	case "add":
		addCalendar()
	case "sync":
		syncCalendars()
	case "desync":
		desyncCalendars()
	case "cleanup":
		cleanupCalendars()
	case "list":
		listCalendars()
	case "reconcile":
		reconcileCalendars()
	default:
		fmt.Printf("Unknown command: %s\n", command)
		os.Exit(1)
	}
}
