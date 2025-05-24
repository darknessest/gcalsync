package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

func eventInPast(e *calendar.Event) bool {
	var (
		ts    string
		layout string
	)
	if e.End != nil && e.End.DateTime != "" {
		ts = e.End.DateTime
		layout = time.RFC3339
	} else if e.End != nil && e.End.Date != "" {
		ts = e.End.Date
		layout = "2006-01-02"
	} else {
		return false // can’t determine → keep
	}
	t, err := time.Parse(layout, ts)
	if err != nil {
		return false
	}
	return t.Before(time.Now())
}

func cleanupCalendars() {
	config, err := readConfig(".gcalsync.toml")
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	db, err := openDB(".gcalsync.db")
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer db.Close()

	calendars := getCalendarsFromDB(db)

	ctx := context.Background()

	for accountName, calendarIDs := range calendars {
		client := getClient(ctx, oauthConfig, db, accountName, config)
		calendarService, err := calendar.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			log.Fatalf("Error creating calendar client: %v", err)
		}

		for _, calendarID := range calendarIDs {
			fmt.Printf("🧹 Cleaning up calendar: %s\n", calendarID)
			cleanupCalendar(calendarService, calendarID, db)
		}
	}

	fmt.Println("Calendars desynced successfully")
}

func cleanupCalendar(calendarService *calendar.Service, calendarID string, db *sql.DB) {
	pageToken := ""

	for {
		events, err := calendarService.Events.List(calendarID).
			PageToken(pageToken).
			SingleEvents(true).
			OrderBy("startTime").
			Do()
		if err != nil {
			log.Fatalf("Error retrieving events: %v", err)
		}

		for _, event := range events.Items {
			if isBlockerEvent(event) && eventInPast(event) {
				err := calendarService.Events.Delete(calendarID, event.Id).Do()
				if err != nil {
					log.Fatalf("Error deleting blocker event: %v", err)
				}
				fmt.Printf("🗑️  Deleted past blocker %s from %s\n", event.Summary, calendarID)
				db.Exec("DELETE FROM blocker_events WHERE calendar_id = ? AND event_id = ?", calendarID, event.Id)
			}
		}

		pageToken = events.NextPageToken
		if pageToken == "" {
			break
		}
	}
}
