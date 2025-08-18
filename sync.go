// sync.go
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

// isBlockerEvent returns true if the event is a gcalsync-generated event
func isBlockerEvent(e *calendar.Event) bool {
	if e.ExtendedProperties != nil && e.ExtendedProperties.Private != nil {
		if v, ok := e.ExtendedProperties.Private["gcalsync_blocker"]; ok && v == "1" {
			return true
		}
		if _, ok := e.ExtendedProperties.Private["gcalsync_travel"]; ok { // new
			return true
		}
	}
	return false
}

// findExistingCopiedEvent attempts to locate an existing gcalsync-managed event in the destination
// calendar when we have no matching record in the local DB. It searches using the private extended
// properties we set on every copied event. If found, its ID is returned, otherwise an empty string.
func findExistingCopiedEvent(srv *calendar.Service, calendarID, originEventID, eventType string) (string, error) {
	// Decide which extended property we should look for
	var propFilter string
	switch eventType {
	case "blocker":
		propFilter = "gcalsync_blocker=1"
	default: // travel_before / travel_after
		propFilter = fmt.Sprintf("gcalsync_travel=%s", eventType)
	}

	pageToken := ""
	for {
		call := srv.Events.List(calendarID).
			PrivateExtendedProperty(propFilter).
			PageToken(pageToken)
		// Narrow down the result set a little – the API allows filtering by the origin ID we embed
		// in every copied event. Older events may not have that yet, but that's fine.
		call = call.Q(originEventID)

		res, err := call.Do()
		if err != nil {
			return "", err
		}
		for _, ev := range res.Items {
			if ev.ExtendedProperties != nil && ev.ExtendedProperties.Private != nil {
				if ev.ExtendedProperties.Private["gcalsync_origin_event_id"] == originEventID {
					return ev.Id, nil
				}
			}
		}
		pageToken = res.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return "", nil
}

func syncCalendars() {
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

	// Build one calendar service per account and reuse it
	services := make(map[string]*calendar.Service)
	for accountName := range calendars {
		client := getClient(ctx, oauthConfig, db, accountName, config)
		srv, err := calendar.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			log.Fatalf("Error creating calendar client: %v", err)
		}
		services[accountName] = srv
	}

	// Optional pre-sync reconciliation with remote calendars
	if config.Sync.ReconcileRemote {
		fmt.Println("🔍 Pre-sync reconciliation enabled. Backfilling local DB from remote…")
		reconcileExistingRemoteEvents(db, services, calendars, config)
	}

	fmt.Println("🚀 Starting calendar synchronization...")
	for accountName, calendarIDs := range calendars {
		fmt.Printf("📅 Syncing calendars for account: %s\n", accountName)
		for _, calendarID := range calendarIDs {
			fmt.Printf("  ↪️ Syncing calendar: %s\n", calendarID)
			syncCalendar(db, services, calendarID, calendars, accountName, config)
		}
		fmt.Println("✅ Calendar synchronization completed successfully!")
	}

	fmt.Println("Calendars synced successfully")
}

func getCalendarsFromDB(db *sql.DB) map[string][]string {
	calendars := make(map[string][]string)
	rows, _ := db.Query("SELECT account_name, calendar_id FROM calendars")
	defer rows.Close()
	for rows.Next() {
		var accountName, calendarID string
		if err := rows.Scan(&accountName, &calendarID); err != nil {
			log.Fatalf("Error scanning calendar row: %v", err)
		}
		calendars[accountName] = append(calendars[accountName], calendarID)
	}
	return calendars
}

func syncCalendar(db *sql.DB, services map[string]*calendar.Service,
	calendarID string, calendars map[string][]string, accountName string,
	cfg *Config) {
	originService := services[accountName]
	pageToken := ""

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
	default: // "future"
		timeMin = now.Format(time.RFC3339)
		timeMax = now.Add(rng).Format(time.RFC3339)
	}

	var allEventsId = map[string]bool{}

	for {
		fmt.Printf("    📥 Retrieving events for calendar: %s\n", calendarID)
		events, err := originService.Events.List(calendarID).
			PageToken(pageToken).
			SingleEvents(true).
			TimeMin(timeMin).
			TimeMax(timeMax).
			OrderBy("startTime").
			Do()
		if err != nil {
			log.Fatalf("Error retrieving events: %v", err)
		}

		for _, event := range events.Items {
			allEventsId[event.Id] = true
			// Google marks "working locations" as events, but we don't want to sync them
			if event.EventType == "workingLocation" {
				continue
			}
			if isBlockerEvent(event) { // skip our own blocker events
				continue
			}
			// Compute blocker event name (formerly called "summary")
			eventName := event.Summary
			var blockerName string
			if cfg.General.EventVisibility == "private" {
				blockerName = strings.ReplaceAll(cfg.General.PrivateEventName, "{name}", eventName)
			} else {
				blockerName = fmt.Sprintf("O_o %s", eventName)
			}
			// Determine description based on config
			description := event.Description
			if cfg.General.DisableDescriptionCopy {
				description = ""
			}
			fmt.Printf("    ✨ Syncing event: %s\n", event.Summary)
			for otherAccountName, calendarIDs := range calendars {
				for _, otherCalendarID := range calendarIDs {
					if otherCalendarID != calendarID {
						/* 1. -------- BLOCKER EVENT (existing logic, only SQL changed) ---------- */
						var existingBlockerEventID string
						var last_updated string
						var originCalendarID string
						var responseStatus string
						err := db.QueryRow(`SELECT event_id, last_updated, origin_calendar_id, response_status
										 FROM blocker_events
										 WHERE calendar_id = ? AND origin_event_id = ? AND event_type='blocker'`,
							otherCalendarID, event.Id).Scan(&existingBlockerEventID, &last_updated, &originCalendarID, &responseStatus)

						// Get original event's response status for the calendar owner
						originalResponseStatus := "accepted" // default
						if event.Attendees != nil {
							for _, attendee := range event.Attendees {
								if attendee.Email == calendarID {
									originalResponseStatus = attendee.ResponseStatus
									break
								}
							}
						}

						// Only skip if event exists, is up to date, and response status hasn't changed
						if err == nil && last_updated == event.Updated && originCalendarID == calendarID && responseStatus == originalResponseStatus {
							fmt.Printf("      ⚠️ Blocker event already exists for origin event ID %s in calendar %s and up to date\n", event.Id, otherCalendarID)
							continue
						}

						otherCalendarService := services[otherAccountName]

						if event.End == nil {
							startTime, _ := time.Parse(time.RFC3339, event.Start.DateTime)
							duration := time.Hour
							endTime := startTime.Add(duration)
							event.End = &calendar.EventDateTime{DateTime: endTime.Format(time.RFC3339)}
						}

						blockerEvent := &calendar.Event{
							Summary:     blockerName,
							Description: description,
							Start:       event.Start,
							End:         event.End,
							Attendees: []*calendar.EventAttendee{
								{
									Email:          otherCalendarID,
									ResponseStatus: originalResponseStatus,
								},
							},
							ExtendedProperties: &calendar.EventExtendedProperties{
								Private: map[string]string{
									"gcalsync_blocker":         "1",
									"gcalsync_origin_event_id": event.Id,
								},
							},
						}
						if cfg.General.DisableReminders {
							blockerEvent.Reminders = nil
						}

						blockerEvent.Visibility = cfg.General.EventVisibility

						// If the DB doesn't know about the copied event, try to locate it directly in the destination calendar
						if existingBlockerEventID == "" {
							if foundID, ferr := findExistingCopiedEvent(otherCalendarService, otherCalendarID, event.Id, "blocker"); ferr == nil {
								existingBlockerEventID = foundID
							}
						}

						var res *calendar.Event

						if existingBlockerEventID != "" {
							res, err = otherCalendarService.Events.Update(otherCalendarID, existingBlockerEventID, blockerEvent).Do()
						} else {
							res, err = otherCalendarService.Events.Insert(otherCalendarID, blockerEvent).Do()
						}
						if err == nil {
							fmt.Printf("      ➕ Blocker event created or updated: %s (Response: %s)\n", blockerEvent.Summary, originalResponseStatus)
							fmt.Printf("      📅 Destination calendar: %s\n", otherCalendarID)
							result, err := db.Exec(`INSERT OR REPLACE INTO blocker_events
								(event_id, origin_calendar_id, calendar_id, account_name,
								 origin_event_id, last_updated, response_status, event_type)
								VALUES (?, ?, ?, ?, ?, ?, ?, 'blocker')`,
								res.Id, calendarID, otherCalendarID, otherAccountName,
								event.Id, event.Updated, originalResponseStatus)
							if err != nil {
								log.Printf("Error inserting blocker event into database: %v\n", err)
							} else {
								rowsAffected, _ := result.RowsAffected()
								fmt.Printf("      📥 Blocker event inserted into database. Rows affected: %d\n", rowsAffected)
							}
						}

						if err != nil {
							log.Fatalf("Error creating blocker event: %v", err)
						}
						/* 2. -------- TRAVEL EVENTS (new) ---------- */
						if cfg.Travel.Enable && event.Location != "" && event.Start != nil && event.End != nil {
							createOrUpdateTravel := func(travelKind string, start, end time.Time, name string) {
								var existingID, lu, ocid, rs string
								q := `SELECT event_id, last_updated, origin_calendar_id, response_status
								      FROM blocker_events
								      WHERE calendar_id = ? AND origin_event_id = ? AND event_type = ?`
								_ = db.QueryRow(q, otherCalendarID, event.Id, travelKind).Scan(&existingID, &lu, &ocid, &rs)
								if lu == event.Updated && ocid == calendarID && rs == originalResponseStatus {
									fmt.Printf("      ⚠️ Travel %s event already exists for origin event ID %s in calendar %s and up to date\n",
										strings.TrimPrefix(travelKind, "travel_"), event.Id, otherCalendarID)
									return
								}

								tEvent := &calendar.Event{
									Summary:     name,
									Description: description,
									Start:       &calendar.EventDateTime{DateTime: start.Format(time.RFC3339)},
									End:         &calendar.EventDateTime{DateTime: end.Format(time.RFC3339)},
									Attendees: []*calendar.EventAttendee{
										{Email: otherCalendarID, ResponseStatus: originalResponseStatus},
									},
									ExtendedProperties: &calendar.EventExtendedProperties{
										Private: map[string]string{
											"gcalsync_travel":          travelKind,
											"gcalsync_origin_event_id": event.Id,
										},
									},
								}
								if cfg.General.DisableReminders {
									tEvent.Reminders = nil
								}
								tEvent.Visibility = cfg.Travel.EventVisibility

								// If DB lacks record, attempt to locate the travel event in destination calendar
								if existingID == "" {
									if foundID, ferr := findExistingCopiedEvent(otherCalendarService, otherCalendarID, event.Id, travelKind); ferr == nil {
										existingID = foundID
									}
								}

								var resp *calendar.Event
								if existingID != "" {
									resp, err = otherCalendarService.Events.Update(otherCalendarID, existingID, tEvent).Do()
								} else {
									resp, err = otherCalendarService.Events.Insert(otherCalendarID, tEvent).Do()
								}
								if err != nil {
									log.Fatalf("Error creating travel event: %v", err)
								}
								fmt.Printf("      🏃 Travel %s event created or updated: %s\n",
									strings.TrimPrefix(travelKind, "travel_"), tEvent.Summary)
								fmt.Printf("      📅 Destination calendar: %s\n", otherCalendarID)

								result, dbErr := db.Exec(`INSERT OR REPLACE INTO blocker_events
								        (event_id, origin_calendar_id, calendar_id, account_name,
								         origin_event_id, last_updated, response_status, event_type)
								        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
									resp.Id, calendarID, otherCalendarID, otherAccountName,
									event.Id, event.Updated, originalResponseStatus, travelKind)
								if dbErr != nil {
									log.Printf("Error inserting travel event into database: %v\n", dbErr)
								} else {
									rowsAffected, _ := result.RowsAffected()
									fmt.Printf("      📥 Travel event inserted into database. Rows affected: %d\n", rowsAffected)
								}
							}

							// compute times
							origStart, _ := time.Parse(time.RFC3339, event.Start.DateTime)
							origEnd, _ := time.Parse(time.RFC3339, event.End.DateTime)
							beforeStart := origStart.Add(-time.Duration(cfg.Travel.MinutesBefore) * time.Minute)
							beforeEnd := origStart
							afterStart := origEnd
							afterEnd := origEnd.Add(time.Duration(cfg.Travel.MinutesAfter) * time.Minute)

							// Build travel event names using name-based templates only
							beforeTmpl := cfg.Travel.BeforeNameTmpl
							afterTmpl := cfg.Travel.AfterNameTmpl

							beforeName := strings.ReplaceAll(beforeTmpl, "{name}", eventName)
							afterName := strings.ReplaceAll(afterTmpl, "{name}", eventName)

							createOrUpdateTravel("travel_before", beforeStart, beforeEnd, beforeName)
							createOrUpdateTravel("travel_after", afterStart, afterEnd, afterName)
						}
					}
				}
			}
		}
		pageToken = events.NextPageToken
		if pageToken == "" {
			break
		}
	}

	// Delete blocker events that not exists from this calendar in other calendars
	fmt.Printf("    🗑 Deleting blocker events that no longer exist in calendar %s from other calendars…\n", calendarID)
	for otherAccountName, calendarIDs := range calendars {
		for _, otherCalendarID := range calendarIDs {
			if otherCalendarID != calendarID {
				otherCalendarService := services[otherAccountName]

				rows, err := db.Query(`SELECT event_id, origin_event_id
				                       FROM blocker_events
				                       WHERE calendar_id = ? AND origin_calendar_id = ?`,
					otherCalendarID, calendarID)
				if err != nil {
					log.Fatalf("Error retrieving blocker events: %v", err)
				}
				eventsToDelete := make([]string, 0)

				defer rows.Close()
				for rows.Next() {
					var eventID string
					var originEventID string
					if err := rows.Scan(&eventID, &originEventID); err != nil {
						log.Fatalf("Error scanning blocker event row: %v", err)
					}

					if val := allEventsId[originEventID]; !val {

						res, err := originService.Events.Get(calendarID, originEventID).Do()
						if err != nil || res == nil || res.Status == "cancelled" {
							fmt.Printf("    🚩 Event marked for deletion: %s\n", eventID)
							eventsToDelete = append(eventsToDelete, eventID)
						}
					}
				}

				for _, eventID := range eventsToDelete {
					fmt.Printf("      🗑 Deleting blocker event: %s\n", eventID)
					res, err := otherCalendarService.Events.Get(otherCalendarID, eventID).Do()

					alreadyDeleted := false

					if err != nil {
						alreadyDeleted = strings.Contains(err.Error(), "410")
						if !alreadyDeleted {
							log.Fatalf("Error retrieving blocker event: %v", err)
						}
					}

					if !alreadyDeleted {
						err = otherCalendarService.Events.Delete(otherCalendarID, eventID).Do()
						if err != nil {
							if res.Status != "cancelled" {
								log.Fatalf("Error deleting blocker event: %v", err)
							} else {
								fmt.Printf("     ❗️ Event already deleted in the other calendar: %s\n", eventID)
							}
						}
					}
					_, err = db.Exec("DELETE FROM blocker_events WHERE event_id = ?", eventID)
					if err != nil {
						log.Fatalf("Error deleting blocker event from database: %v", err)
					}

					fmt.Printf("      ✅ Blocker event deleted: %s\n", res.Summary)
				}
			}
		}
	}
}
