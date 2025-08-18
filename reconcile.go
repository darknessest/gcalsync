package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// reconcileCalendars performs a one-off reconciliation pass to backfill the local DB
// with gcalsync-managed events that already exist in remote calendars.
func reconcileCalendars() {
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
	services := make(map[string]*calendar.Service)
	for accountName := range calendars {
		client := getClient(ctx, oauthConfig, db, accountName, config)
		srv, err := calendar.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			log.Fatalf("Error creating calendar client: %v", err)
		}
		services[accountName] = srv
	}

	fmt.Println("🔄 Reconciling existing remote gcalsync-managed events into local DB…")
	reconcileExistingRemoteEvents(db, services, calendars, config)
	fmt.Println("✅ Reconciliation completed")
}

// reconcileExistingRemoteEvents scans destination calendars for events marked with our
// extended properties and ensures the local DB has corresponding rows to prevent duplicates.
func reconcileExistingRemoteEvents(db *sql.DB, services map[string]*calendar.Service, calendars map[string][]string, cfg *Config) {
	// Build time window like in sync
	now := time.Now()
	rng := time.Duration(cfg.Sync.TimeframeDays) * 24 * time.Hour
	var timeMin, timeMax string
	switch strings.ToLower(cfg.Sync.Direction) {
	case "past":
		timeMin = now.Add(-rng).Format(time.RFC3339)
		timeMax = now.Format(time.RFC3339)
	case "all":
		timeMin = now.Add(-rng).Format(time.RFC3339)
		timeMax = now.Add(rng).Format(time.RFC3339)
	default:
		timeMin = now.Format(time.RFC3339)
		timeMax = now.Add(rng).Format(time.RFC3339)
	}

	filters := []struct {
		Prop      string
		EventType string
	}{
		{Prop: "gcalsync_blocker=1", EventType: "blocker"},
		{Prop: "gcalsync_travel=travel_before", EventType: "travel_before"},
		{Prop: "gcalsync_travel=travel_after", EventType: "travel_after"},
	}

	for destAccountName, destCalendarIDs := range calendars {
		destService := services[destAccountName]
		for _, destCalendarID := range destCalendarIDs {
			fmt.Printf("  📥 Reconciling in calendar: %s\n", destCalendarID)
			for _, f := range filters {
				pageToken := ""
				for {
					call := destService.Events.List(destCalendarID).
						PrivateExtendedProperty(f.Prop).
						SingleEvents(true).
						TimeMin(timeMin).
						TimeMax(timeMax).
						PageToken(pageToken)
					res, err := call.Do()
					if err != nil {
						log.Fatalf("Error listing events for reconciliation: %v", err)
					}
					for _, ev := range res.Items {
						if ev.Status == "cancelled" || ev.ExtendedProperties == nil || ev.ExtendedProperties.Private == nil {
							continue
						}
						originEventID := ev.ExtendedProperties.Private["gcalsync_origin_event_id"]
						if originEventID == "" {
							continue
						}

						originCalendarID := ""
						originOwnerStatus := "accepted"
						foundOrigin := false
						for originAccountName, originCalendarIDs := range calendars {
							srv := services[originAccountName]
							for _, ocid := range originCalendarIDs {
								if ocid == destCalendarID {
									continue
								}
								orig, getErr := srv.Events.Get(ocid, originEventID).Do()
								if getErr == nil && orig != nil && orig.Status != "cancelled" {
									originCalendarID = ocid
									if orig.Attendees != nil {
										for _, a := range orig.Attendees {
											if a.Email == ocid {
												originOwnerStatus = a.ResponseStatus
												break
											}
										}
									}
									foundOrigin = true
									break
								}
							}
							if foundOrigin {
								break
							}
						}

						accountName := getAccountNameByCalendarID(db, destCalendarID)
						_, dbErr := db.Exec(`INSERT OR REPLACE INTO blocker_events
						(event_id, origin_calendar_id, calendar_id, account_name,
						 origin_event_id, last_updated, response_status, event_type)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
							ev.Id, originCalendarID, destCalendarID, accountName,
							originEventID, ev.Updated, originOwnerStatus, f.EventType)
						if dbErr != nil {
							log.Printf("    ⚠️ Failed to upsert reconciled event %s: %v\n", ev.Id, dbErr)
						} else {
							fmt.Printf("    🔗 Reconciled %s for origin %s into DB (dest %s)\n", f.EventType, originEventID, destCalendarID)
						}
					}
					// Handle pagination after processing all items on the page
					pageToken = res.NextPageToken
					if pageToken == "" {
						break
					}
				}
			}
		}
	}
}
