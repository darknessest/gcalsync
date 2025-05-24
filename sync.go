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
	// fallback for events created by older versions
	return strings.Contains(e.Summary, "O_o")
}

func syncCalendars() {
	config, err := readConfig(".gcalsync.toml")
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}
	useReminders       := config.General.DisableReminders
	eventVisibility    := config.General.EventVisibility
	privateEventSummary:= config.General.PrivateEventSummary
	travelCfg          := config.Travel     // <-- NEW

	db, err := openDB(".gcalsync.db")
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer db.Close()

	calendars := getCalendarsFromDB(db)

	ctx := context.Background()
	fmt.Println("🚀 Starting calendar synchronization...")
	for accountName, calendarIDs := range calendars {
		fmt.Printf("📅 Syncing calendars for account: %s\n", accountName)
		client := getClient(ctx, oauthConfig, db, accountName, config)
		calendarService, err := calendar.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			log.Fatalf("Error creating calendar client: %v", err)
		}

		for _, calendarID := range calendarIDs {
			fmt.Printf("  ↪️ Syncing calendar: %s\n", calendarID)
			syncCalendar(db, calendarService, calendarID, calendars, accountName,
				useReminders, eventVisibility, privateEventSummary, travelCfg)
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

func syncCalendar(db *sql.DB, calendarService *calendar.Service,
	calendarID string, calendars map[string][]string, accountName string,
	useReminders bool, eventVisibility string, privateEventSummary string,
	travelCfg TravelConfig) {
	config, err := readConfig(".gcalsync.toml")
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	ctx := context.Background()
	calendarService = tokenExpired(db, accountName, calendarService, ctx)
	pageToken := ""

	now := time.Now()
	startOfCurrentMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	endOfNextMonth := startOfCurrentMonth.AddDate(0, 2, -1)
	timeMin := startOfCurrentMonth.Format(time.RFC3339)
	timeMax := endOfNextMonth.Format(time.RFC3339)

	var allEventsId = map[string]bool{}

	for {
		fmt.Printf("    📥 Retrieving events for calendar: %s\n", calendarID)
		events, err := calendarService.Events.List(calendarID).
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
			var blockerSummary string
			if eventVisibility == "private" {
				// allow user template; {summary} will be replaced by original text
				if privateEventSummary == "" {
					blockerSummary = fmt.Sprintf("O_o %s", event.Summary)
				} else {
					blockerSummary = strings.ReplaceAll(privateEventSummary, "{summary}", event.Summary)
				}
			} else {
				blockerSummary = fmt.Sprintf("O_o %s", event.Summary)
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

						client := getClient(ctx, oauthConfig, db, otherAccountName, config)
						otherCalendarService, err := calendar.NewService(ctx, option.WithHTTPClient(client))
						if err != nil {
							log.Fatalf("Error creating calendar client: %v", err)
						}

						blockerDescription := event.Description

						if event.End == nil {
							startTime, _ := time.Parse(time.RFC3339, event.Start.DateTime)
							duration := time.Hour
							endTime := startTime.Add(duration)
							event.End = &calendar.EventDateTime{DateTime: endTime.Format(time.RFC3339)}
						}

						blockerEvent := &calendar.Event{
							Summary:     blockerSummary,
							Description: blockerDescription,
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
									"gcalsync_blocker": "1",
								},
							},
						}
						if !useReminders {
							blockerEvent.Reminders = nil
						}

						if eventVisibility != "" {
							blockerEvent.Visibility = eventVisibility
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
						if travelCfg.Enable && event.Location != "" && event.Start != nil && event.End != nil {
							createOrUpdateTravel := func(travelKind string, start, end time.Time, summary string) {
								var existingID, lu, ocid, rs string
								q := `SELECT event_id, last_updated, origin_calendar_id, response_status
									  FROM blocker_events
									  WHERE calendar_id = ? AND origin_event_id = ? AND event_type = ?`
								_ = db.QueryRow(q, otherCalendarID, event.Id, travelKind).Scan(&existingID, &lu, &ocid, &rs)
								if lu == event.Updated && ocid == calendarID && rs == originalResponseStatus {
									return // up-to-date
								}

								tEvent := &calendar.Event{
									Summary:     summary,
									Description: event.Description,
									Start: &calendar.EventDateTime{DateTime: start.Format(time.RFC3339)},
									End:   &calendar.EventDateTime{DateTime: end.Format(time.RFC3339)},
									Attendees: []*calendar.EventAttendee{
										{Email: otherCalendarID, ResponseStatus: originalResponseStatus},
									},
									ExtendedProperties: &calendar.EventExtendedProperties{
										Private: map[string]string{"gcalsync_travel": travelKind},
									},
								}
								if !useReminders { tEvent.Reminders = nil }
								vis := travelCfg.EventVisibility
								if vis == "" { vis = eventVisibility }
								if vis != "" { tEvent.Visibility = vis }

								var resp *calendar.Event
								if existingID != "" {
									resp, err = otherCalendarService.Events.Update(otherCalendarID, existingID, tEvent).Do()
								} else {
									resp, err = otherCalendarService.Events.Insert(otherCalendarID, tEvent).Do()
								}
								if err != nil { log.Fatalf("Error creating travel event: %v", err) }

								_, _ = db.Exec(`INSERT OR REPLACE INTO blocker_events
								   (event_id, origin_calendar_id, calendar_id, account_name,
									origin_event_id, last_updated, response_status, event_type)
								   VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
								   resp.Id, calendarID, otherCalendarID, otherAccountName,
								   event.Id, event.Updated, originalResponseStatus, travelKind)
							}

							// compute times
							origStart, _ := time.Parse(time.RFC3339, event.Start.DateTime)
							origEnd,   _ := time.Parse(time.RFC3339, event.End.DateTime)
							beforeStart := origStart.Add(-time.Duration(travelCfg.MinutesBefore) * time.Minute)
							beforeEnd   := origStart
							afterStart  := origEnd
							afterEnd    := origEnd.Add(time.Duration(travelCfg.MinutesAfter) * time.Minute)

							beforeSummary := strings.ReplaceAll(travelCfg.BeforeSummaryTmpl, "{summary}", event.Summary)
							afterSummary  := strings.ReplaceAll(travelCfg.AfterSummaryTmpl,  "{summary}", event.Summary)

							createOrUpdateTravel("travel_before", beforeStart, beforeEnd, beforeSummary)
							createOrUpdateTravel("travel_after",  afterStart,  afterEnd,  afterSummary)
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
				client := getClient(ctx, oauthConfig, db, otherAccountName, config)
				otherCalendarService, err := calendar.NewService(ctx, option.WithHTTPClient(client))
				rows, err := db.Query(`SELECT event_id, origin_event_id, event_type
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

						res, err := calendarService.Events.Get(calendarID, originEventID).Do()
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
