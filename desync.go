package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

func desyncCalendars() {
	config, err := readConfig(".gcalsync.toml")
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	ctx := context.Background()
	db, err := openDB(".gcalsync.db")
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer db.Close()

	fmt.Println("🚀 Starting calendar desynchronization...")

	// First, get all events grouped by account name to avoid token refresh loops
	rows, err := db.Query("SELECT event_id, calendar_id, account_name FROM blocker_events")
	if err != nil {
		log.Fatalf("❌ Error retrieving blocker events from database: %v", err)
	}
	defer rows.Close()

	// Group events by account name
	eventsByAccount := make(map[string][]struct {
		EventID    string
		CalendarID string
	})

	for rows.Next() {
		var eventID, calendarID, accountName string
		if err := rows.Scan(&eventID, &calendarID, &accountName); err != nil {
			log.Fatalf("❌ Error scanning blocker event row: %v", err)
		}

		eventsByAccount[accountName] = append(eventsByAccount[accountName], struct {
			EventID    string
			CalendarID string
		}{EventID: eventID, CalendarID: calendarID})
	}

	// Process events by account to reuse calendar services
	for accountName, events := range eventsByAccount {
		fmt.Printf("📅 Processing events for account: %s\n", accountName)

		// Create calendar service once per account (like in sync.go)
		client := getClient(ctx, oauthConfig, db, accountName, config)
		calendarService, err := calendar.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			log.Fatalf("❌ Error creating calendar client: %v", err)
		}

		// Process all events for this account
		for _, event := range events {
			err = calendarService.Events.Delete(event.CalendarID, event.EventID).Do()
			if err != nil {
				if googleErr, ok := err.(*googleapi.Error); ok && (googleErr.Code == 404 || googleErr.Code == 410) {
					fmt.Printf("  ⚠️ Blocker event not found in calendar (or already deleted): %s\n", event.EventID)
				} else {
					log.Fatalf("❌ Error deleting blocker event: %v", err)
				}
			} else {
				fmt.Printf("  ✅ Blocker event deleted: %s\n", event.EventID)
			}
		}
	}

	// Delete all blocker events from the database
	_, err = db.Exec("DELETE FROM blocker_events")
	if err != nil {
		log.Fatalf("❌ Error deleting blocker events from database: %v", err)
	} else {
		fmt.Printf("  📥 All blocker events deleted from database\n")
	}

	fmt.Println("Calendars desynced successfully")
}

func getAccountNameByCalendarID(db *sql.DB, calendarID string) string {
	var accountName string
	err := db.QueryRow("SELECT account_name FROM calendars WHERE calendar_id = ?", calendarID).Scan(&accountName)
	if err != nil {
		log.Fatalf("Error retrieving account name for calendar ID %s: %v", calendarID, err)
	}
	return accountName
}
